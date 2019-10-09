// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package conn

import (
	"database/sql"

	tcontext "github.com/pingcap/dm/pkg/context"
	"github.com/pingcap/dm/pkg/log"
	"github.com/pingcap/dm/pkg/retry"
	"github.com/pingcap/dm/pkg/terror"
	"github.com/pingcap/dm/pkg/utils"

	"go.uber.org/zap"
)

// BaseConn wraps a connection to DB
// For Syncer and Loader Unit, they both have different amount of connections due to config
// Currently we have some types of connections exist
// Syncer:
// 	Worker Connection:
//      DML connection:
// 			execute some DML on Downstream DB, one unit has `syncer.WorkerCount` worker connections
//  	DDL Connection:
// 			execute some DDL on Downstream DB, one unit has one connection
// 	CheckPoint Connection:
// 		interact with CheckPoint DB, one unit has one connection
//  OnlineDDL connection:
// 		interact with Online DDL DB, one unit has one connection
//  ShardGroupKeeper connection:
// 		interact with ShardGroupKeeper DB, one unit has one connection
//
// Loader:
// 	Worker Connection:
// 		execute some DML to Downstream DB, one unit has `loader.PoolSize` worker connections
// 	CheckPoint Connection:
// 		interact with CheckPoint DB, one unit has one connection
// 	restore Connection:
// 		only use to create schema and table in restoreData,
// 		it ignore already exists error and it should be removed after use, one unit has one connection
//
// each connection should have ability to retry on some common errors (e.g. tmysql.ErrTiKVServerTimeout) or maybe some specify errors in the future
// and each connection also should have ability to reset itself during some specify connection error (e.g. driver.ErrBadConn)
type BaseConn struct {
	DBConn *sql.Conn

	RetryStrategy retry.Strategy
}

// newBaseConn builds BaseConn to connect real DB
func newBaseConn(conn *sql.Conn, strategy retry.Strategy) *BaseConn {
	if strategy == nil {
		strategy = &retry.FiniteRetryStrategy{}
	}
	return &BaseConn{conn, strategy}
}

// SetRetryStrategy set retry strategy for baseConn
func (conn *BaseConn) SetRetryStrategy(strategy retry.Strategy) error {
	if conn == nil {
		return terror.ErrDBUnExpect.Generate("database connection not valid")
	}
	conn.RetryStrategy = strategy
	return nil
}

// QuerySQL defines query statement, and connect to real DB
func (conn *BaseConn) QuerySQL(tctx *tcontext.Context, query string, args ...interface{}) (*sql.Rows, error) {
	if conn == nil || conn.DBConn == nil {
		return nil, terror.ErrDBUnExpect.Generate("database connection not valid")
	}
	tctx.L().Debug("query statement",
		zap.String("query", utils.TruncateString(query, -1)),
		zap.String("argument", utils.TruncateInterface(args, -1)))

	rows, err := conn.DBConn.QueryContext(tctx.Context(), query, args...)

	if err != nil {
		tctx.L().Error("query statement failed",
			zap.String("query", utils.TruncateString(query, -1)),
			zap.String("argument", utils.TruncateInterface(args, -1)),
			log.ShortError(err))
		return nil, terror.ErrDBQueryFailed.Delegate(err, utils.TruncateString(query, -1))
	}
	return rows, nil
}

// ExecuteSQLWithIgnoreError executes sql on real DB, and will ignore some error and continue execute the next query.
// return
// 1. failed: (the index of sqls executed error, error)
// 2. succeed: (len(sqls), nil)
func (conn *BaseConn) ExecuteSQLWithIgnoreError(tctx *tcontext.Context, ignoreErr func(error) bool, queries []string, args ...[]interface{}) (int, error) {
	if len(queries) == 0 {
		return 0, nil
	}
	if conn == nil || conn.DBConn == nil {
		return 0, terror.ErrDBUnExpect.Generate("database connection not valid")
	}

	txn, err := conn.DBConn.BeginTx(tctx.Context(), nil)

	if err != nil {
		return 0, terror.ErrDBExecuteFailed.Delegate(err, "begin")
	}

	l := len(queries)

	for i, query := range queries {
		var arg []interface{}
		if len(args) > i {
			arg = args[i]
		}

		tctx.L().Debug("execute statement",
			zap.String("query", utils.TruncateString(query, -1)),
			zap.String("argument", utils.TruncateInterface(arg, -1)))

		_, err = txn.ExecContext(tctx.Context(), query, arg...)
		if err != nil {
			if ignoreErr != nil && ignoreErr(err) {
				tctx.L().Warn("execute statement failed and will ignore this error",
					zap.String("query", utils.TruncateString(query, -1)),
					zap.String("argument", utils.TruncateInterface(arg, -1)),
					log.ShortError(err))
				continue
			}

			tctx.L().Error("execute statement failed",
				zap.String("query", utils.TruncateString(query, -1)),
				zap.String("argument", utils.TruncateInterface(arg, -1)), log.ShortError(err))

			rerr := txn.Rollback()
			if rerr != nil {
				tctx.L().Error("rollback failed",
					zap.String("query", utils.TruncateString(query, -1)),
					zap.String("argument", utils.TruncateInterface(arg, -1)),
					log.ShortError(rerr))
			}
			// we should return the exec err, instead of the rollback rerr.
			return i, terror.ErrDBExecuteFailed.Delegate(err, utils.TruncateString(query, -1))
		}
	}
	err = txn.Commit()
	if err != nil {
		return l, terror.ErrDBExecuteFailed.Delegate(err, "commit")
	}
	return l, nil
}

// ExecuteSQL executes sql on real DB,
// return
// 1. failed: (the index of sqls executed error, error)
// 2. succeed: (len(sqls), nil)
func (conn *BaseConn) ExecuteSQL(tctx *tcontext.Context, queries []string, args ...[]interface{}) (int, error) {
	return conn.ExecuteSQLWithIgnoreError(tctx, nil, queries, args...)
}

// ApplyRetryStrategy apply specify strategy for BaseConn
func (conn *BaseConn) ApplyRetryStrategy(tctx *tcontext.Context, params retry.Params,
	operateFn func(*tcontext.Context) (interface{}, error)) (interface{}, int, error) {
	return conn.RetryStrategy.Apply(tctx, params, operateFn)
}

func (conn *BaseConn) close() error {
	if conn == nil || conn.DBConn == nil {
		return nil
	}
	return terror.ErrDBUnExpect.Delegate(conn.DBConn.Close(), "close")
}