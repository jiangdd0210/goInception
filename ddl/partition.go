// Copyright 2018 PingCAP, Inc.
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

package ddl

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"github.com/hanchuanchuan/goInception/ast"
	"github.com/hanchuanchuan/goInception/expression"
	"github.com/hanchuanchuan/goInception/meta"
	"github.com/hanchuanchuan/goInception/model"
	"github.com/hanchuanchuan/goInception/mysql"
	"github.com/hanchuanchuan/goInception/parser/opcode"
	"github.com/hanchuanchuan/goInception/sessionctx"
	"github.com/hanchuanchuan/goInception/table"
	"github.com/hanchuanchuan/goInception/types"
	"github.com/hanchuanchuan/goInception/util/chunk"
	"github.com/pingcap/errors"
)

const (
	partitionMaxValue = "MAXVALUE"
	primarykey        = "PRIMARY KEY"
)

// buildTablePartitionInfo builds partition info and checks for some errors.
func buildTablePartitionInfo(ctx sessionctx.Context, d *ddl, s *ast.CreateTableStmt) (*model.PartitionInfo, error) {
	if s.Partition == nil {
		return nil, nil
	}
	pi := &model.PartitionInfo{
		Type:   s.Partition.Tp,
		Enable: ctx.GetSessionVars().EnableTablePartition,
	}
	if s.Partition.Expr != nil {
		buf := new(bytes.Buffer)
		s.Partition.Expr.Format(buf)
		pi.Expr = buf.String()
	} else if s.Partition.ColumnNames != nil {
		pi.Columns = make([]model.CIStr, 0, len(s.Partition.ColumnNames))
		for _, cn := range s.Partition.ColumnNames {
			pi.Columns = append(pi.Columns, cn.Name)
		}
	}
	for _, def := range s.Partition.Definitions {
		comment, _ := def.Comment()
		// TODO: generate multiple global ID for paritions, reduce the times of obtaining the global ID from the storage.
		pid, err := d.genGlobalID()
		if err != nil {
			return nil, errors.Trace(err)
		}

		piDef := model.PartitionDefinition{
			Name:    def.Name,
			ID:      pid,
			Comment: comment,
		}

		if s.Partition.Tp == model.PartitionTypeRange {
			buf := new(bytes.Buffer)
			// Range columns partitions support multi-column partitions.
			for _, expr := range def.Clause.(*ast.PartitionDefinitionClauseLessThan).Exprs {
				expr.Format(buf)
				piDef.LessThan = append(piDef.LessThan, buf.String())
				buf.Reset()
			}
			pi.Definitions = append(pi.Definitions, piDef)
		}
	}
	return pi, nil
}

func checkPartitionNameUnique(tbInfo *model.TableInfo, pi *model.PartitionInfo) error {
	partNames := make(map[string]struct{})
	if tbInfo.Partition != nil {
		oldPars := tbInfo.Partition.Definitions
		for _, oldPar := range oldPars {
			partNames[oldPar.Name.L] = struct{}{}
		}
	}
	newPars := pi.Definitions
	for _, newPar := range newPars {
		if _, ok := partNames[newPar.Name.L]; ok {
			return ErrSameNamePartition.GenWithStackByArgs(newPar.Name)
		}
		partNames[newPar.Name.L] = struct{}{}
	}
	return nil
}

// checkPartitionFuncValid checks partition function validly.
func checkPartitionFuncValid(ctx sessionctx.Context, tblInfo *model.TableInfo, expr ast.ExprNode) error {
	switch v := expr.(type) {
	case *ast.FuncCastExpr, *ast.CaseExpr:
		return errors.Trace(ErrPartitionFunctionIsNotAllowed)
	case *ast.FuncCallExpr:
		// check function which allowed in partitioning expressions
		// see https://dev.mysql.com/doc/mysql-partitioning-excerpt/5.7/en/partitioning-limitations-functions.html
		switch v.FnName.L {
		case ast.Abs, ast.Ceiling, ast.DateDiff, ast.Day, ast.DayOfMonth, ast.DayOfWeek, ast.DayOfYear, ast.Extract, ast.Floor,
			ast.Hour, ast.MicroSecond, ast.Minute, ast.Mod, ast.Month, ast.Quarter, ast.Second, ast.TimeToSec, ast.ToDays,
			ast.ToSeconds, ast.Weekday, ast.Year, ast.YearWeek:
			return nil
		case ast.UnixTimestamp:
			if len(v.Args) == 1 {
				col, err := expression.RewriteSimpleExprWithTableInfo(ctx, tblInfo, v.Args[0])
				if err != nil {
					return errors.Trace(err)
				}
				if col.GetType().Tp != mysql.TypeTimestamp {
					return errors.Trace(errWrongExprInPartitionFunc)
				}
				return nil
			}
		}
		return errors.Trace(ErrPartitionFunctionIsNotAllowed)
	case *ast.BinaryOperationExpr:
		// The DIV operator (opcode.IntDiv) is also supported; the / operator ( opcode.Div ) is not permitted.
		// see https://dev.mysql.com/doc/refman/5.7/en/partitioning-limitations.html
		switch v.Op {
		case opcode.Or, opcode.And, opcode.Xor, opcode.LeftShift, opcode.RightShift, opcode.BitNeg, opcode.Div:
			return errors.Trace(ErrPartitionFunctionIsNotAllowed)
		}
		return nil
	case *ast.UnaryOperationExpr:
		if v.Op == opcode.BitNeg {
			return errors.Trace(ErrPartitionFunctionIsNotAllowed)
		}
	}
	return nil
}

// checkPartitionFuncType checks partition function return type.
func checkPartitionFuncType(ctx sessionctx.Context, s *ast.CreateTableStmt, cols []*table.Column, tblInfo *model.TableInfo) error {
	if s.Partition.Expr == nil {
		return nil
	}
	buf := new(bytes.Buffer)
	s.Partition.Expr.Format(buf)
	exprStr := buf.String()
	if s.Partition.Tp == model.PartitionTypeRange {
		// if partition by columnExpr, check the column type
		if _, ok := s.Partition.Expr.(*ast.ColumnNameExpr); ok {
			for _, col := range cols {
				name := strings.Replace(col.Name.String(), ".", "`.`", -1)
				// Range partitioning key supported types: tinyint, smallint, mediumint, int and bigint.
				if !validRangePartitionType(col) && fmt.Sprintf("`%s`", name) == exprStr {
					return errors.Trace(ErrNotAllowedTypeInPartition.GenWithStackByArgs(exprStr))
				}
			}
		}
	}

	e, err := expression.ParseSimpleExprWithTableInfo(ctx, buf.String(), tblInfo)
	if err != nil {
		return errors.Trace(err)
	}
	if e.GetType().EvalType() == types.ETInt {
		return nil
	}
	return ErrPartitionFuncNotAllowed.GenWithStackByArgs("PARTITION")
}

// checkCreatePartitionValue checks whether `less than value` is strictly increasing for each partition.
// Side effect: it may simplify the partition range definition from a constant expression to an integer.
func checkCreatePartitionValue(ctx sessionctx.Context, tblInfo *model.TableInfo, pi *model.PartitionInfo, cols []*table.Column) error {
	defs := pi.Definitions
	if len(defs) <= 1 {
		return nil
	}

	if strings.EqualFold(defs[len(defs)-1].LessThan[0], partitionMaxValue) {
		defs = defs[:len(defs)-1]
	}
	isUnsignedBigint := isRangePartitionColUnsignedBigint(cols, pi)
	var prevRangeValue interface{}
	for i := 0; i < len(defs); i++ {
		if strings.EqualFold(defs[i].LessThan[0], partitionMaxValue) {
			return errors.Trace(ErrPartitionMaxvalue)
		}

		currentRangeValue, fromExpr, err := getRangeValue(ctx, tblInfo, defs[i].LessThan[0], isUnsignedBigint)
		if err != nil {
			return errors.Trace(err)
		}
		if fromExpr {
			// Constant fold the expression.
			defs[i].LessThan[0] = fmt.Sprintf("%d", currentRangeValue)
		}

		if i == 0 {
			prevRangeValue = currentRangeValue
			continue
		}

		if isUnsignedBigint {
			if currentRangeValue.(uint64) <= prevRangeValue.(uint64) {
				return errors.Trace(ErrRangeNotIncreasing)
			}
		} else {
			if currentRangeValue.(int64) <= prevRangeValue.(int64) {
				return errors.Trace(ErrRangeNotIncreasing)
			}
		}
		prevRangeValue = currentRangeValue
	}
	return nil
}

// getRangeValue gets an integer from the range value string.
// The returned boolean value indicates whether the input string is a constant expression.
func getRangeValue(ctx sessionctx.Context, tblInfo *model.TableInfo, str string, unsignedBigint bool) (interface{}, bool, error) {
	// Unsigned bigint was converted to uint64 handle.
	if unsignedBigint {
		if value, err := strconv.ParseUint(str, 10, 64); err == nil {
			return value, false, nil
		}

		if e, err1 := expression.ParseSimpleExprWithTableInfo(ctx, str, tblInfo); err1 == nil {
			res, isNull, err2 := e.EvalInt(ctx, chunk.Row{})
			if err2 == nil && isNull == false {
				return uint64(res), true, nil
			}
		}
	} else {
		if value, err := strconv.ParseInt(str, 10, 64); err == nil {
			return value, false, nil
		}
		// The range value maybe not an integer, it could be a constant expression.
		// For example, the following two cases are the same:
		// PARTITION p0 VALUES LESS THAN (TO_SECONDS('2004-01-01'))
		// PARTITION p0 VALUES LESS THAN (63340531200)
		if e, err1 := expression.ParseSimpleExprWithTableInfo(ctx, str, tblInfo); err1 == nil {
			res, isNull, err2 := e.EvalInt(ctx, chunk.Row{})
			if err2 == nil && isNull == false {
				return res, true, nil
			}
		}
	}
	return 0, false, ErrNotAllowedTypeInPartition.GenWithStackByArgs(str)
}

// validRangePartitionType checks the type supported by the range partitioning key.
func validRangePartitionType(col *table.Column) bool {
	switch col.FieldType.EvalType() {
	case types.ETInt:
		return true
	default:
		return false
	}
}

// checkDropTablePartition checks if the partition exists and does not allow deleting the last existing partition in the table.
func checkDropTablePartition(meta *model.TableInfo, partLowerNames []string) error {
	pi := meta.Partition
	oldDefs := pi.Definitions
	for _, pn := range partLowerNames {
		found := false
		for _, def := range oldDefs {
			if def.Name.L == pn {
				found = true
				break
			}
		}
		if !found {
			return errors.Trace(ErrDropPartitionNonExistent.GenWithStackByArgs(pn))
		}
	}
	if len(oldDefs) == len(partLowerNames) {
		return errors.Trace(ErrDropLastPartition)
	}
	return nil
}

// removePartitionInfo each ddl job deletes a partition.
func removePartitionInfo(tblInfo *model.TableInfo, partName string) int64 {
	oldDefs := tblInfo.Partition.Definitions
	newDefs := make([]model.PartitionDefinition, 0, len(oldDefs)-1)
	var pid int64
	for i := 0; i < len(oldDefs); i++ {
		if !strings.EqualFold(oldDefs[i].Name.L, strings.ToLower(partName)) {
			continue
		}
		pid = oldDefs[i].ID
		newDefs = append(oldDefs[:i], oldDefs[i+1:]...)
		break
	}
	tblInfo.Partition.Definitions = newDefs
	return pid
}

// onDropTablePartition deletes old partition meta.
func onDropTablePartition(t *meta.Meta, job *model.Job) (ver int64, _ error) {
	var partName string
	if err := job.DecodeArgs(&partName); err != nil {
		job.State = model.JobStateCancelled
		return ver, errors.Trace(err)
	}
	var partNames []string
	if partName == "" {
		if err := job.DecodeArgs(&partNames); err != nil {
			job.State = model.JobStateCancelled
			return ver, errors.Trace(err)
		}
	} else {
		partNames = []string{strings.ToLower(partName)}
	}
	tblInfo, err := getTableInfo(t, job, job.SchemaID)
	if err != nil {
		return ver, errors.Trace(err)
	}
	// If an error occurs, it returns that it cannot delete all partitions or that the partition doesn't exist.
	err = checkDropTablePartition(tblInfo, partNames)
	if err != nil {
		job.State = model.JobStateCancelled
		return ver, errors.Trace(err)
	}
	physicalTableID := removePartitionInfo(tblInfo, partName)
	ver, err = updateVersionAndTableInfo(t, job, tblInfo, true)
	if err != nil {
		return ver, errors.Trace(err)
	}

	// Finish this job.
	job.FinishTableJob(model.JobStateDone, model.StateNone, ver, tblInfo)
	// A background job will be created to delete old partition data.
	job.Args = []interface{}{physicalTableID}
	return ver, nil
}

func checkAddPartitionTooManyPartitions(piDefs int) error {
	if piDefs > PartitionCountLimit {
		return ErrTooManyPartitions
	}
	return nil
}

func getPartitionIDs(table *model.TableInfo) []int64 {
	if table.GetPartitionInfo() == nil {
		return []int64{}
	}
	physicalTableIDs := make([]int64, 0, len(table.Partition.Definitions))
	for _, def := range table.Partition.Definitions {
		physicalTableIDs = append(physicalTableIDs, def.ID)
	}
	return physicalTableIDs
}

// checkRangePartitioningKeysConstraints checks that the range partitioning key is included in the table constraint.
func checkRangePartitioningKeysConstraints(ctx sessionctx.Context, s *ast.CreateTableStmt, tblInfo *model.TableInfo, constraints []*ast.Constraint) error {
	// Returns directly if there is no constraint in the partition table.
	// TODO: Remove the test 's.Partition.Expr == nil' when we support 'PARTITION BY RANGE COLUMNS'
	if len(constraints) == 0 || s.Partition.Expr == nil {
		return nil
	}

	// Extract the column names in table constraints to []map[string]struct{}.
	consColNames := extractConstraintsColumnNames(constraints)

	// Parse partitioning key, extract the column names in the partitioning key to slice.
	buf := new(bytes.Buffer)
	s.Partition.Expr.Format(buf)
	var partkeys []string
	e, err := expression.ParseSimpleExprWithTableInfo(ctx, buf.String(), tblInfo)
	if err != nil {
		return errors.Trace(err)
	}
	cols := expression.ExtractColumns(e)
	for _, col := range cols {
		partkeys = append(partkeys, col.ColName.L)
	}

	// Checks that the partitioning key is included in the constraint.
	for _, con := range consColNames {
		// Every unique key on the table must use every column in the table's partitioning expression.
		// See https://dev.mysql.com/doc/refman/5.7/en/partitioning-limitations-partitioning-keys-unique-keys.html.
		if !checkConstraintIncludePartKey(partkeys, con) {
			return ErrUniqueKeyNeedAllFieldsInPf.GenWithStackByArgs(primarykey)
		}
	}
	return nil
}

// extractConstraintsColumnNames extract the column names in table constraints to []map[string]struct{}.
func extractConstraintsColumnNames(cons []*ast.Constraint) []map[string]struct{} {
	var constraints []map[string]struct{}
	for _, v := range cons {
		if v.Tp == ast.ConstraintUniq || v.Tp == ast.ConstraintPrimaryKey {
			uniKeys := make(map[string]struct{})
			for _, key := range v.Keys {
				uniKeys[key.Column.Name.L] = struct{}{}
			}
			// Extract every unique key and primary key.
			if len(uniKeys) != 0 {
				constraints = append(constraints, uniKeys)
			}
		}
	}
	return constraints
}

// checkConstraintIncludePartKey checks that the partitioning key is included in the constraint.
func checkConstraintIncludePartKey(partkeys []string, constraints map[string]struct{}) bool {
	for _, pk := range partkeys {
		if _, ok := constraints[pk]; !ok {
			return false
		}
	}
	return true
}

// isRangePartitionColUnsignedBigint returns true if the partitioning key column type is unsigned bigint type.
func isRangePartitionColUnsignedBigint(cols []*table.Column, pi *model.PartitionInfo) bool {
	for _, col := range cols {
		isUnsigned := col.Tp == mysql.TypeLonglong && mysql.HasUnsignedFlag(col.Flag)
		if isUnsigned && strings.Contains(strings.ToLower(pi.Expr), col.Name.L) {
			return true
		}
	}
	return false
}
