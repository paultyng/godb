package godb

import (
	"database/sql"
	"time"

	"gitlab.com/samonzeweb/godb/adapters"
)

// selectStatement is a SELECT sql statement builder.
type selectStatement struct {
	db *DB

	distinct   bool
	columns    []string
	fromTables []string
	joins      []*joinPart
	where      []*Condition
	groupBy    []string
	having     []*Condition
	orderBy    []string
	limit      *int
	offset     *int
	suffixes   []string
}

// joinPart describes a sql JOIN clause.
type joinPart struct {
	joinType  string
	tableName string
	as        string
	on        *Condition
}

// pointersGetter is a func type, returning a list of pointers (and error) for
// a given instance pointer and a columns names list.
type pointersGetter func(record interface{}, columns []string) ([]interface{}, error)

// SelectFrom initializes a SELECT statement builder.
func (db *DB) SelectFrom(tableName string) *selectStatement {
	ss := &selectStatement{db: db}
	return ss.From(tableName)
}

// From adds table to the select statement. It can be called multiple times.
func (ss *selectStatement) From(tableName string) *selectStatement {
	ss.fromTables = append(ss.fromTables, tableName)
	return ss
}

// Columns adds columns to select.
func (ss *selectStatement) Columns(columns ...string) *selectStatement {
	ss.columns = append(ss.columns, columns...)
	return ss
}

// Distinct adds DISTINCT keyword the the generated statement.
func (ss *selectStatement) Distinct() *selectStatement {
	ss.distinct = true
	return ss
}

// LeftJoin adds a LEFT JOIN clause, wich will be inserted between FROM and WHERE
// clauses.
func (ss *selectStatement) LeftJoin(tableName string, as string, on *Condition) *selectStatement {
	join := &joinPart{
		joinType:  "LEFT JOIN",
		tableName: tableName,
		as:        as,
		on:        on,
	}
	ss.joins = append(ss.joins, join)
	return ss
}

// Where adds a condition using string and arguments.
func (ss *selectStatement) Where(sql string, args ...interface{}) *selectStatement {
	return ss.WhereQ(Q(sql, args...))
}

// WhereQ adds a simple or complex predicate generated with Q and
// conjunctions.
func (ss *selectStatement) WhereQ(condition *Condition) *selectStatement {
	ss.where = append(ss.where, condition)
	return ss
}

// GroupBy adds a GROUP BY clause.
func (ss *selectStatement) GroupBy(groupBy string) *selectStatement {
	ss.groupBy = append(ss.groupBy, groupBy)
	return ss
}

// Having adds a HAVING clause with a condition build with a sql string and
// its arguments (like Where).
func (ss *selectStatement) Having(sql string, args ...interface{}) *selectStatement {
	return ss.HavingQ(Q(sql, args...))
}

// HavingQ adds a simple or complex predicate generated with Q and
// conjunctions (like WhereQ).
func (ss *selectStatement) HavingQ(condition *Condition) *selectStatement {
	ss.having = append(ss.having, condition)
	return ss
}

// OrderBy adds an expression for the ORDER BY clause.
func (ss *selectStatement) OrderBy(orderBy string) *selectStatement {
	ss.orderBy = append(ss.orderBy, orderBy)
	return ss
}

// Offset specifies the value for the OFFSET clause.
func (ss *selectStatement) Offset(offset int) *selectStatement {
	ss.offset = new(int)
	*ss.offset = offset
	return ss
}

// Limit specifies the value for the LIMIT clause.
func (ss *selectStatement) Limit(limit int) *selectStatement {
	ss.limit = new(int)
	*ss.limit = limit
	return ss
}

// Suffix adds an expression to suffix the query.
func (ss *selectStatement) Suffix(suffix string) *selectStatement {
	ss.suffixes = append(ss.suffixes, suffix)
	return ss
}

// ToSQL returns a string with the SQL request (containing placeholders),
// the arguments slices, and an error.
func (ss *selectStatement) ToSQL() (string, []interface{}, error) {
	sqlWhereLength, argsWhereLength, err := sumOfConditionsLengths(ss.where)
	if err != nil {
		return "", nil, err
	}

	sqlHavingLength, argsHavingLength, err := sumOfConditionsLengths(ss.having)
	if err != nil {
		return "", nil, err
	}

	sqlBuffer := newSQLBuffer(
		ss.db.adapter,
		sqlWhereLength+sqlHavingLength+64,
		argsWhereLength+argsHavingLength+4,
	)

	sqlBuffer.write("SELECT ")

	if ss.distinct == true {
		sqlBuffer.write("DISTINCT ")
	}

	if err := sqlBuffer.writeColumns(ss.columns); err != nil {
		return "", nil, err
	}

	if err := sqlBuffer.writeFrom(ss.fromTables...); err != nil {
		return "", nil, err
	}

	if err := sqlBuffer.writeJoins(ss.joins); err != nil {
		return "", nil, err
	}

	if err := sqlBuffer.writeWhere(ss.where); err != nil {
		return "", nil, err
	}

	if err := sqlBuffer.writeGroupByAndHaving(ss.groupBy, ss.having); err != nil {
		return "", nil, err
	}

	if err := sqlBuffer.writeOrderBy(ss.orderBy); err != nil {
		return "", nil, err
	}

	offsetFirst := false
	if limitOffsetOrderer, ok := ss.db.adapter.(adapters.LimitOffsetOrderer); ok {
		offsetFirst = limitOffsetOrderer.IsOffsetFirst()
	}
	if offsetFirst {
		// Offset is before limit
		if err := sqlBuffer.writeOffset(ss.offset); err != nil {
			return "", nil, err
		}

		if err := sqlBuffer.writeLimit(ss.limit); err != nil {
			return "", nil, err
		}
	} else {
		// Limit is before offset (default case)
		if err := sqlBuffer.writeLimit(ss.limit); err != nil {
			return "", nil, err
		}

		if err := sqlBuffer.writeOffset(ss.offset); err != nil {
			return "", nil, err
		}
	}

	if err := sqlBuffer.writeStrings(ss.suffixes); err != nil {
		return "", nil, err
	}

	return sqlBuffer.sqlString(), sqlBuffer.sqlArguments(), nil
}

// Do executes the select statement.
// The record argument has to be a pointer to a struct or a slice.
func (ss *selectStatement) Do(record interface{}) error {
	recordInfo, err := buildRecordDescription(record)
	if err != nil {
		return err
	}

	// the function which will return the pointers according to the given columns
	f := func(record interface{}, columns []string) ([]interface{}, error) {
		pointers, err := recordInfo.structMapping.GetPointersForColumns(record, columns...)
		return pointers, err
	}

	return ss.do(recordInfo, f)
}

// do executes the statement and fill the struct or slice given through the
// recordDescription.
func (ss *selectStatement) do(recordInfo *recordDescription, pointersGetter pointersGetter) error {
	if recordInfo.isSlice == false {
		// Only one row is requested
		ss.Limit(1)
		// Some DB require an offset if a limit is specified (MS SQL Server)
		if ss.offset == nil {
			ss.Offset(0)
		}
		// Some DB require an order by if offset and limit are used
		// (still MS SQL Server)
		if len(ss.orderBy) == 0 {
			keysColumns := recordInfo.structMapping.GetKeyColumnsNames()
			for _, keyColumn := range keysColumns {
				ss.OrderBy(keyColumn)
			}
		}
	}

	sqlQuery, args, err := ss.ToSQL()
	if err != nil {
		return err
	}
	sqlQuery = ss.db.replacePlaceholders(sqlQuery)
	ss.db.logPrintln("SELECT : ", sqlQuery, args)

	startTime := time.Now()
	queryable, err := ss.db.getQueryable(sqlQuery)
	if err != nil {
		return err
	}
	rows, err := queryable.Query(args...)
	condumedTime := timeElapsedSince(startTime)
	ss.db.addConsumedTime(condumedTime)
	ss.db.logDuration(condumedTime)
	if err != nil {
		ss.db.logPrintln("ERROR : ", err)
		return err
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		ss.db.logPrintln("ERROR : ", err)
		return err
	}

	var rowsCount int
	for rows.Next() {
		rowsCount++
		err = recordInfo.fillRecord(
			// Fill one instance with one row
			func(record interface{}) error {
				fieldsPointers, innererr := pointersGetter(record, columns)
				if innererr != nil {
					return innererr
				}
				innererr = rows.Scan(fieldsPointers...)
				if err != nil {
					return innererr
				}
				return nil
			})

		if err != nil {
			ss.db.logPrintln("ERROR : ", err)
			return err
		}
	}

	err = rows.Err()
	if err != nil {
		ss.db.logPrintln("ERROR : ", err)
	}

	// When a single instance is requested but not found, sql.ErrNoRows is
	// returned like QueryRow in database/sql package.
	if recordInfo.isSlice == false && rowsCount == 0 {
		err = sql.ErrNoRows
	}

	return err
}

// Count runs the request with COUNT(*) (remove others columns)
// and returns the count.
func (ss *selectStatement) Count() (int64, error) {
	ss.columns = ss.columns[:0]
	ss.Columns("COUNT(*)")

	sql, args, err := ss.ToSQL()
	if err != nil {
		return 0, err
	}
	sql = ss.db.replacePlaceholders(sql)
	ss.db.logPrintln("SELECT : ", sql, args)

	var count int64
	startTime := time.Now()
	queryable, err := ss.db.getQueryable(sql)
	if err != nil {
		return 0, err
	}
	err = queryable.QueryRow(args...).Scan(&count)
	condumedTime := timeElapsedSince(startTime)
	ss.db.addConsumedTime(condumedTime)
	ss.db.logDuration(condumedTime)
	if err != nil {
		ss.db.logPrintln("ERROR : ", err)
		return 0, err
	}

	return count, nil
}
