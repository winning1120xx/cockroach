// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package norm

import (
	"github.com/cockroachdb/cockroach/pkg/sql/opt"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/cat"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/memo"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/props/physical"
	_ "github.com/cockroachdb/cockroach/pkg/sql/sem/builtins" // register all builtins in builtins:init() for memo package
	"github.com/cockroachdb/cockroach/pkg/sql/sem/builtins/builtinsregistry"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/eval"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/buildutil"
	"github.com/cockroachdb/cockroach/pkg/util/errorutil"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/redact"
)

// ReplaceFunc is the callback function passed to the Factory.Replace method.
// It is called with each child of the expression passed to Replace. See the
// Replace method for more details.
type ReplaceFunc func(e opt.Expr) opt.Expr

// MatchedRuleFunc defines the callback function for the NotifyOnMatchedRule
// event supported by the optimizer and factory. It is invoked each time an
// optimization rule (Normalize or Explore) has been matched. The name of the
// matched rule is passed as a parameter. If the function returns false, then
// the rule is not applied (i.e. skipped).
type MatchedRuleFunc func(ruleName opt.RuleName) bool

// AppliedRuleFunc defines the callback function for the NotifyOnAppliedRule
// event supported by the optimizer and factory. It is invoked each time an
// optimization rule (Normalize or Explore) has been applied.
//
// The function is called with the name of the rule and the expressions it
// affected. For a normalization rule, the source is always nil, and the target
// is the expression constructed by the replace pattern. For an exploration
// rule, the source is the expression matched by the rule, and the target is
// the first expression constructed by the replace pattern. If no expressions
// were constructed, it is nil. Additional expressions beyond the first can be
// accessed by following the NextExpr links on the target expression.
type AppliedRuleFunc func(ruleName opt.RuleName, source, target opt.Expr)

// Factory constructs a normalized expression tree within the memo. As each
// kind of expression is constructed by the factory, it transitively runs
// normalization transformations defined for that expression type. This may
// result in the construction of a different type of expression than what was
// requested. If, after normalization, the expression is already part of the
// memo, then construction is a no-op. Otherwise, a new memo group is created,
// with the normalized expression as its first and only expression.
//
// Factory is largely auto-generated by optgen. The generated code can be found
// in factory.og.go. The factory.go file contains helper functions that are
// invoked by normalization patterns. While most patterns are specified in the
// Optgen DSL, the factory always calls the `onConstruct` method as its last
// step, in order to allow any custom manual code to execute.
type Factory struct {
	evalCtx *eval.Context

	// mem is the Memo data structure that the factory builds.
	mem *memo.Memo

	// funcs is the struct used to call all custom match and replace functions
	// used by the normalization rules.
	funcs CustomFuncs

	// matchedRule is the callback function that is invoked each time a normalize
	// rule has been matched by the factory. It can be set via a call to the
	// NotifyOnMatchedRule method.
	matchedRule MatchedRuleFunc

	// appliedRule is the callback function which is invoked each time a normalize
	// rule has been applied by the factory. It can be set via a call to the
	// NotifyOnAppliedRule method.
	appliedRule AppliedRuleFunc

	// catalog is the opt catalog, used to resolve names during constant folding
	// of special metadata queries like 'table_name'::regclass.
	catalog cat.Catalog

	// See FoldingControl.
	foldingControl FoldingControl

	// constructorStackDepth tracks the call stack depth of factory constructor
	// methods. It is incremented when a constructor function is called, and
	// decremented when a constructor function returns.
	constructorStackDepth int

	// disabledRules is a set of rules that are not allowed to run, used when
	// rules are disabled during testing to prevent rule cycles.
	disabledRules util.FastIntSet
}

// maxConstructorStackDepth is the maximum allowed depth of a constructor call
// stack. Optgen generates factory code that refers to this constant.
//
// If constructorStackDepth exceeds this limit, a rule cycle likely exists that
// will cause a stack overflow. To avoid a stack overflow, no further
// normalization rules are applied when this limit is reached, and the
// onMaxConstructorStackDepthExceeded method is called. This can result in an
// expression that is not fully optimized.
const maxConstructorStackDepth = 10_000

// Injecting this builtins dependency in the init function allows the memo
// package to access builtin properties without importing the builtins package.
func init() {
	memo.GetBuiltinProperties = builtinsregistry.GetBuiltinProperties
}

// Init initializes a Factory structure with a new, blank memo structure inside.
// This must be called before the factory can be used (or reused).
//
// By default, a factory only constant-folds immutable operators; this can be
// changed using FoldingControl().AllowStableFolds().
func (f *Factory) Init(evalCtx *eval.Context, catalog cat.Catalog) {
	// Initialize (or reinitialize) the memo.
	mem := f.mem
	if mem == nil {
		mem = &memo.Memo{}
	}
	mem.Init(evalCtx)

	// This initialization pattern ensures that fields are not unwittingly
	// reused. Field reuse must be explicit.
	*f = Factory{
		mem:     mem,
		evalCtx: evalCtx,
		catalog: catalog,
	}

	f.funcs.Init(f)
	f.foldingControl.DisallowStableFolds()
}

// FoldingControl returns the FoldingControl instance for this factory.
func (f *Factory) FoldingControl() *FoldingControl {
	return &f.foldingControl
}

// DetachMemo extracts the memo from the optimizer, and then re-initializes the
// factory so that its reuse will not impact the detached memo. This method is
// used to extract a read-only memo during the PREPARE phase.
//
// Before extracting the memo, DetachMemo first clears all column statistics in
// the memo. This is used to free up the potentially large amount of memory
// used by histograms. This does not affect the quality of the plan used at
// execution time, since the stats are just recalculated anyway when
// placeholders are assigned. If there are no placeholders, there is no need
// for column statistics, since the memo is already fully optimized.
func (f *Factory) DetachMemo() *memo.Memo {
	m := f.mem
	f.mem = nil
	m.Detach()
	f.Init(f.evalCtx, nil /* catalog */)
	return m
}

// DisableOptimizations disables all transformation rules. The unaltered input
// expression tree becomes the output expression tree (because no transforms
// are applied).
func (f *Factory) DisableOptimizations() {
	f.NotifyOnMatchedRule(func(opt.RuleName) bool { return false })
}

// DisableOptimizationsTemporarily disables all transformation rules during the
// execution of the given function fn. A MatchedRuleFunc previously set by
// NotifyOnMatchedRule is not invoked during execution of fn, but will be
// invoked for future rule matches after fn returns.
func (f *Factory) DisableOptimizationsTemporarily(fn func()) {
	originalMatchedRule := f.matchedRule
	f.DisableOptimizations()
	fn()
	f.matchedRule = originalMatchedRule
}

// NotifyOnMatchedRule sets a callback function which is invoked each time a
// normalize rule has been matched by the factory. If matchedRule is nil, then
// no further notifications are sent, and all rules are applied by default. In
// addition, callers can invoke the DisableOptimizations convenience method to
// disable all rules.
func (f *Factory) NotifyOnMatchedRule(matchedRule MatchedRuleFunc) {
	f.matchedRule = matchedRule
}

// NotifyOnAppliedRule sets a callback function which is invoked each time a
// normalize rule has been applied by the factory. If appliedRule is nil, then
// no further notifications are sent.
func (f *Factory) NotifyOnAppliedRule(appliedRule AppliedRuleFunc) {
	f.appliedRule = appliedRule
}

// SetDisabledRules is used to prevent normalization rule cycles when rules are
// disabled during testing. SetDisabledRules does not prevent rules from
// matching - rather, it notifies the Factory that rules have been prevented
// from matching using NotifyOnMatchedRule.
func (f *Factory) SetDisabledRules(disabledRules util.FastIntSet) {
	f.disabledRules = disabledRules
}

// Memo returns the memo structure that the factory is operating upon.
func (f *Factory) Memo() *memo.Memo {
	return f.mem
}

// Metadata returns the query-specific metadata, which includes information
// about the columns and tables used in this particular query.
func (f *Factory) Metadata() *opt.Metadata {
	return f.mem.Metadata()
}

// CustomFuncs returns the set of custom functions used by normalization rules.
func (f *Factory) CustomFuncs() *CustomFuncs {
	return &f.funcs
}

// EvalContext returns the *eval.Context of the factory.
func (f *Factory) EvalContext() *eval.Context {
	return f.evalCtx
}

// CopyAndReplace builds this factory's memo by constructing a copy of a subtree
// that is part of another memo. That memo's metadata is copied to this
// factory's memo so that tables and columns referenced by the copied memo can
// keep the same ids. The copied subtree becomes the root of the destination
// memo, having the given physical properties.
//
// The "replace" callback function allows the caller to override the default
// traversal and cloning behavior with custom logic. It is called for each node
// in the "from" subtree, and has the choice of constructing an arbitrary
// replacement node, or delegating to the default behavior by calling
// CopyAndReplaceDefault, which constructs a copy of the source operator using
// children returned by recursive calls to the replace callback. Note that if a
// non-leaf replacement node is constructed, its inputs must be copied using
// CopyAndReplaceDefault.
//
// Sample usage:
//
//	var replaceFn ReplaceFunc
//	replaceFn = func(e opt.Expr) opt.Expr {
//	  if e.Op() == opt.PlaceholderOp {
//	    return f.ConstructConst(evalPlaceholder(e))
//	  }
//
//	  // Copy e, calling replaceFn on its inputs recursively.
//	  return f.CopyAndReplaceDefault(e, replaceFn)
//	}
//
//	f.CopyAndReplace(from, fromProps, replaceFn)
//
// NOTE: Callers must take care to always create brand new copies of non-
// singleton source nodes rather than referencing existing nodes. The source
// memo should always be treated as immutable, and the destination memo must be
// completely independent of it once CopyAndReplace has completed.
func (f *Factory) CopyAndReplace(
	from memo.RelExpr, fromProps *physical.Required, replace ReplaceFunc,
) {
	if !f.mem.IsEmpty() {
		panic(errors.AssertionFailedf("destination memo must be empty"))
	}

	// Copy the next scalar rank to the target memo so that new scalar
	// expressions built with the new memo will not share scalar ranks with
	// existing expressions.
	f.mem.CopyNextRankFrom(from.Memo())

	// Copy all metadata to the target memo so that referenced tables and
	// columns can keep the same ids they had in the "from" memo. Scalar
	// expressions in the metadata cannot have placeholders, so we simply copy
	// the expressions without replacement.
	f.mem.Metadata().CopyFrom(from.Memo().Metadata(), f.CopyWithoutAssigningPlaceholders)

	// Perform copy and replacement, and store result as the root of this
	// factory's memo.
	to := f.invokeReplace(from, replace).(memo.RelExpr)
	f.Memo().SetRoot(to, fromProps)
}

// CopyWithoutAssigningPlaceholders returns a copy of the given scalar expression.
// It does not attempt to replace placeholders with values.
func (f *Factory) CopyWithoutAssigningPlaceholders(e opt.Expr) opt.Expr {
	return f.CopyAndReplaceDefault(e, f.CopyWithoutAssigningPlaceholders)
}

// AssignPlaceholders is used just before execution of a prepared Memo. It makes
// a copy of the given memo, but with any placeholder values replaced by their
// assigned values. This can trigger additional normalization rules that can
// substantially rewrite the tree. Once all placeholders are assigned, the
// exploration phase can begin.
func (f *Factory) AssignPlaceholders(from *memo.Memo) (err error) {
	defer func() {
		if r := recover(); r != nil {
			// This code allows us to propagate errors without adding lots of checks
			// for `if err != nil` throughout the construction code. This is only
			// possible because the code does not update shared state and does not
			// manipulate locks.
			if ok, e := errorutil.ShouldCatch(r); ok {
				err = e
			} else {
				panic(r)
			}
		}
	}()

	// Copy the "from" memo to this memo, replacing any Placeholder operators as
	// the copy proceeds.
	var replaceFn ReplaceFunc
	replaceFn = func(e opt.Expr) opt.Expr {
		if placeholder, ok := e.(*memo.PlaceholderExpr); ok {
			d, err := eval.Expr(f.evalCtx.Context, f.evalCtx, e.(*memo.PlaceholderExpr).Value)
			if err != nil {
				panic(err)
			}
			return f.ConstructConstVal(d, placeholder.DataType())
		}
		return f.CopyAndReplaceDefault(e, replaceFn)
	}
	f.CopyAndReplace(from.RootExpr().(memo.RelExpr), from.RootProps(), replaceFn)

	return nil
}

// CheckConstructorStackDepth panics in test builds if the constructor stack
// depth is not zero. The stack depth should be 0 after a top-level constructor
// function returns. It is used to verify that the stack depth is correctly
// decremented for each constructor function.
func (f *Factory) CheckConstructorStackDepth() {
	if buildutil.CrdbTestBuild && f.constructorStackDepth != 0 {
		panic(errors.AssertionFailedf(
			"expected constructor stack depth %v to be 0",
			f.constructorStackDepth,
		))
	}
}

// onMaxConstructorStackDepthExceeded is called when constructorStackDepth
// exceeds maxConstructorStackDepth. In test builds it panics. In release builds
// it reports an error to Sentry to alert of a likely normalization rule cycle.
func (f *Factory) onMaxConstructorStackDepthExceeded() {
	err := errors.AssertionFailedf(
		"optimizer factory constructor call stack exceeded max depth of %v",
		maxConstructorStackDepth,
	)
	if buildutil.CrdbTestBuild {
		panic(err)
	}
	errorutil.SendReport(f.evalCtx.Ctx(), &f.evalCtx.Settings.SV, err)
}

// onConstructRelational is called as a final step by each factory method that
// constructs a relational expression, so that any custom manual pattern
// matching/replacement code can be run.
func (f *Factory) onConstructRelational(rel memo.RelExpr) memo.RelExpr {
	// [SimplifyZeroCardinalityGroup]
	// SimplifyZeroCardinalityGroup replaces a group with [0 - 0] cardinality
	// with an empty values expression. It is placed here because it depends on
	// the logical properties of the group in question.
	if rel.Op() != opt.ValuesOp {
		relational := rel.Relational()
		// We can do this if we only contain leakproof operators. As an example of
		// an immutable operator that should not be folded: a Limit on top of an
		// empty input has to error out if the limit turns out to be negative.
		if relational.Cardinality.IsZero() && relational.VolatilitySet.IsLeakproof() {
			if f.matchedRule == nil || f.matchedRule(opt.SimplifyZeroCardinalityGroup) {
				values := f.funcs.ConstructEmptyValues(relational.OutputCols)
				if f.appliedRule != nil {
					f.appliedRule(opt.SimplifyZeroCardinalityGroup, nil, values)
				}
				return values
			}
		}
	}

	return rel
}

// onConstructScalar is called as a final step by each factory method that
// constructs a scalar expression, so that any custom manual pattern matching/
// replacement code can be run.
func (f *Factory) onConstructScalar(scalar opt.ScalarExpr) opt.ScalarExpr {
	return scalar
}

// ----------------------------------------------------------------------
//
// Convenience construction methods.
//
// ----------------------------------------------------------------------

// ConstructZeroValues constructs a Values operator with zero rows and zero
// columns. It is used to create a dummy input for operators like CreateTable.
func (f *Factory) ConstructZeroValues() memo.RelExpr {
	return f.ConstructValues(memo.EmptyScalarListExpr, &memo.ValuesPrivate{
		Cols: opt.ColList{},
		ID:   f.Metadata().NextUniqueID(),
	})
}

// ConstructJoin constructs the join operator that corresponds to the given join
// operator type.
func (f *Factory) ConstructJoin(
	joinOp opt.Operator, left, right memo.RelExpr, on memo.FiltersExpr, private *memo.JoinPrivate,
) memo.RelExpr {
	switch joinOp {
	case opt.InnerJoinOp:
		return f.ConstructInnerJoin(left, right, on, private)
	case opt.InnerJoinApplyOp:
		return f.ConstructInnerJoinApply(left, right, on, private)
	case opt.LeftJoinOp:
		return f.ConstructLeftJoin(left, right, on, private)
	case opt.LeftJoinApplyOp:
		return f.ConstructLeftJoinApply(left, right, on, private)
	case opt.RightJoinOp:
		return f.ConstructRightJoin(left, right, on, private)
	case opt.FullJoinOp:
		return f.ConstructFullJoin(left, right, on, private)
	case opt.SemiJoinOp:
		return f.ConstructSemiJoin(left, right, on, private)
	case opt.SemiJoinApplyOp:
		return f.ConstructSemiJoinApply(left, right, on, private)
	case opt.AntiJoinOp:
		return f.ConstructAntiJoin(left, right, on, private)
	case opt.AntiJoinApplyOp:
		return f.ConstructAntiJoinApply(left, right, on, private)
	}
	panic(errors.AssertionFailedf("unexpected join operator: %v", redact.Safe(joinOp)))
}

// ConstructConstVal constructs one of the constant value operators from the
// given datum value. While most constants are represented with Const, there are
// special-case operators for True, False, and Null, to make matching easier.
// Null operators require the static type to be specified, so that rewrites do
// not change it.
func (f *Factory) ConstructConstVal(d tree.Datum, t *types.T) opt.ScalarExpr {
	if d == tree.DNull {
		return f.ConstructNull(t)
	}
	if boolVal, ok := d.(*tree.DBool); ok {
		// Map True/False datums to True/False operator.
		if *boolVal {
			return memo.TrueSingleton
		}
		return memo.FalseSingleton
	}
	return f.ConstructConst(d, t)
}

// ConstructConstFilter builds a filter that constrains the given column to one
// of the given set of constant values. This is performed by either constructing
// an equality expression or an IN expression.
func (f *Factory) ConstructConstFilter(col opt.ColumnID, values tree.Datums) memo.FiltersItem {
	if len(values) == 1 {
		return f.ConstructFiltersItem(f.ConstructEq(
			f.ConstructVariable(col),
			f.ConstructConstVal(values[0], values[0].ResolvedType()),
		))
	}
	elems := make(memo.ScalarListExpr, len(values))
	elemTypes := make([]*types.T, len(values))
	for i := range values {
		typ := values[i].ResolvedType()
		elems[i] = f.ConstructConstVal(values[i], typ)
		elemTypes[i] = typ
	}
	return f.ConstructFiltersItem(f.ConstructIn(
		f.ConstructVariable(col),
		f.ConstructTuple(elems, types.MakeTuple(elemTypes)),
	))
}

// ----------------------------------------------------------------------
//
// Convenience functions.
//
// ----------------------------------------------------------------------

// RemapCols remaps columns IDs in the input ScalarExpr by replacing occurrences
// of the keys of colMap with the corresponding values. If column IDs are
// encountered in the input ScalarExpr that are not keys in colMap, they are not
// remapped.
func (f *Factory) RemapCols(scalar opt.ScalarExpr, colMap opt.ColMap) opt.ScalarExpr {
	// Recursively walk the scalar sub-tree looking for references to columns
	// that need to be replaced and then replace them appropriately.
	var replace ReplaceFunc
	replace = func(e opt.Expr) opt.Expr {
		switch t := e.(type) {
		case *memo.VariableExpr:
			dstCol, ok := colMap.Get(int(t.Col))
			if !ok {
				// The column ID is not in colMap so no replacement is required.
				return e
			}
			return f.ConstructVariable(opt.ColumnID(dstCol))
		}
		return f.Replace(e, replace)
	}

	return replace(scalar).(opt.ScalarExpr)
}
