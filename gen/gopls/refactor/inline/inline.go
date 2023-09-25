// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package inline

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	pathpkg "path"
	"reflect"
	"strconv"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/types/typeutil"
	"golang.org/x/tools/imports"
	"github.com/walteh/retab/gen/gopls/typeparams"
)

// A Caller describes the function call and its enclosing context.
//
// The client is responsible for populating this struct and passing it to Inline.
type Caller struct {
	Fset    *token.FileSet
	Types   *types.Package
	Info    *types.Info
	File    *ast.File
	Call    *ast.CallExpr
	Content []byte // source of file containing

	path []ast.Node
}

// Inline inlines the called function (callee) into the function call (caller)
// and returns the updated, formatted content of the caller source file.
//
// Inline does not mutate any public fields of Caller or Callee.
//
// The log records the decision-making process.
//
// TODO(adonovan): provide an API for clients that want structured
// output: a list of import additions and deletions plus one or more
// localized diffs (or even AST transformations, though ownership and
// mutation are tricky) near the call site.
func Inline(logf func(string, ...any), caller *Caller, callee *Callee) ([]byte, error) {
	logf("inline %s @ %v",
		debugFormatNode(caller.Fset, caller.Call),
		caller.Fset.PositionFor(caller.Call.Lparen, false))

	if !consistentOffsets(caller) {
		return nil, fmt.Errorf("internal error: caller syntax positions are inconsistent with file content (did you forget to use FileSet.PositionFor when computing the file name?)")
	}

	res, err := inline(logf, caller, &callee.impl)
	if err != nil {
		return nil, err
	}

	// Replace the call (or some node that encloses it) by new syntax.
	assert(res.old != nil, "old is nil")
	assert(res.new != nil, "new is nil")

	// Don't call replaceNode(caller.File, res.old, res.new)
	// as it mutates the caller's syntax tree.
	// Instead, splice the file, replacing the extent of the "old"
	// node by a formatting of the "new" node, and re-parse.
	// We'll fix up the imports on this new tree, and format again.
	var f *ast.File
	{
		start := offsetOf(caller.Fset, res.old.Pos())
		end := offsetOf(caller.Fset, res.old.End())
		var out bytes.Buffer
		out.Write(caller.Content[:start])
		// TODO(adonovan): might it make more sense to use
		// callee.Fset when formatting res.new??
		if err := format.Node(&out, caller.Fset, res.new); err != nil {
			return nil, err
		}
		out.Write(caller.Content[end:])
		const mode = parser.ParseComments | parser.SkipObjectResolution | parser.AllErrors
		f, err = parser.ParseFile(caller.Fset, "callee.go", &out, mode)
		if err != nil {
			// Something has gone very wrong.
			logf("failed to parse <<%s>>", &out) // debugging
			return nil, err
		}
	}

	// Add new imports.
	//
	// Insert new imports after last existing import,
	// to avoid migration of pre-import comments.
	// The imports will be organized below.
	if len(res.newImports) > 0 {
		var importDecl *ast.GenDecl
		if len(f.Imports) > 0 {
			// Append specs to existing import decl
			importDecl = f.Decls[0].(*ast.GenDecl)
		} else {
			// Insert new import decl.
			importDecl = &ast.GenDecl{Tok: token.IMPORT}
			f.Decls = prepend[ast.Decl](importDecl, f.Decls...)
		}
		for _, spec := range res.newImports {
			// Check that the new imports are accessible.
			path, _ := strconv.Unquote(spec.Path.Value)
			if !canImport(caller.Types.Path(), path) {
				return nil, fmt.Errorf("can't inline function %v as its body refers to inaccessible package %q", callee, path)
			}
			importDecl.Specs = append(importDecl.Specs, spec)
		}
	}

	var out bytes.Buffer
	if err := format.Node(&out, caller.Fset, f); err != nil {
		return nil, err
	}
	newSrc := out.Bytes()

	// Remove imports that are no longer referenced.
	//
	// It ought to be possible to compute the set of PkgNames used
	// by the "old" code, compute the free identifiers of the
	// "new" code using a syntax-only (no go/types) algorithm, and
	// see if the reduction in the number of uses of any PkgName
	// equals the number of times it appears in caller.Info.Uses,
	// indicating that it is no longer referenced by res.new.
	//
	// However, the notorious ambiguity of resolving T{F: 0} makes this
	// unreliable: without types, we can't tell whether F refers to
	// a field of struct T, or a package-level const/var of a
	// dot-imported (!) package.
	//
	// So, for now, we run imports.Process, which is
	// unsatisfactory as it has to run the go command, and it
	// looks at the user's module cache state--unnecessarily,
	// since this step cannot add new imports.
	//
	// TODO(adonovan): replace with a simpler implementation since
	// all the necessary imports are present but merely untidy.
	// That will be faster, and also less prone to nondeterminism
	// if there are bugs in our logic for import maintenance.
	//
	// However, github.com/walteh/retab/gen/gopls/imports.ApplyFixes is
	// too simple as it requires the caller to have figured out
	// all the logical edits. In our case, we know all the new
	// imports that are needed (see newImports), each of which can
	// be specified as:
	//
	//   &imports.ImportFix{
	//     StmtInfo: imports.ImportInfo{path, name,
	//     IdentName: name,
	//     FixType:   imports.AddImport,
	//   }
	//
	// but we don't know which imports are made redundant by the
	// inlining itself. For example, inlining a call to
	// fmt.Println may make the "fmt" import redundant.
	//
	// Also, both imports.Process and internal/imports.ApplyFixes
	// reformat the entire file, which is not ideal for clients
	// such as gopls. (That said, the point of a canonical format
	// is arguably that any tool can reformat as needed without
	// this being inconvenient.)
	//
	// We could invoke imports.Process and parse its result,
	// compare against the original AST, compute a list of import
	// fixes, and return that too.

	// Recompute imports only if there were existing ones.
	if len(f.Imports) > 0 {
		formatted, err := imports.Process("output", newSrc, nil)
		if err != nil {
			logf("cannot reformat: %v <<%s>>", err, &out)
			return nil, err // cannot reformat (a bug?)
		}
		newSrc = formatted
	}
	return newSrc, nil
}

type result struct {
	newImports []*ast.ImportSpec
	old, new   ast.Node // e.g. replace call expr by callee function body expression
}

// inline returns a pair of an old node (the call, or something
// enclosing it) and a new node (its replacement, which may be a
// combination of caller, callee, and new nodes), along with the set
// of new imports needed.
//
// TODO(adonovan): rethink the 'result' interface. The assumption of a
// one-to-one replacement seems fragile. One can easily imagine the
// transformation replacing the call and adding new variable
// declarations, for example, or replacing a call statement by zero or
// many statements.)
//
// TODO(adonovan): in earlier drafts, the transformation was expressed
// by splicing substrings of the two source files because syntax
// trees don't preserve comments faithfully (see #20744), but such
// transformations don't compose. The current implementation is
// tree-based but is very lossy wrt comments. It would make a good
// candidate for evaluating an alternative fully self-contained tree
// representation, such as any proposed solution to #20744, or even
// dst or some private fork of go/ast.)
func inline(logf func(string, ...any), caller *Caller, callee *gobCallee) (*result, error) {
	checkInfoFields(caller.Info)

	// Inlining of dynamic calls is not currently supported,
	// even for local closure calls. (This would be a lot of work.)
	calleeSymbol := typeutil.StaticCallee(caller.Info, caller.Call)
	if calleeSymbol == nil {
		// e.g. interface method
		return nil, fmt.Errorf("cannot inline: not a static function call")
	}

	// Reject cross-package inlining if callee has
	// free references to unexported symbols.
	samePkg := caller.Types.Path() == callee.PkgPath
	if !samePkg && len(callee.Unexported) > 0 {
		return nil, fmt.Errorf("cannot inline call to %s because body refers to non-exported %s",
			callee.Name, callee.Unexported[0])
	}

	// -- analyze callee's free references in caller context --

	// syntax path enclosing Call, innermost first (Path[0]=Call)
	caller.path, _ = astutil.PathEnclosingInterval(caller.File, caller.Call.Pos(), caller.Call.End())

	// Find the outermost function enclosing the call site (if any).
	// Analyze all its local vars for the "single assignment" property
	// (Taking the address &v counts as a potential assignment.)
	var assign1 func(v *types.Var) bool // reports whether v a single-assignment local var
	{
		updatedLocals := make(map[*types.Var]bool)
		for _, n := range caller.path {
			if decl, ok := n.(*ast.FuncDecl); ok {
				escape(caller.Info, decl.Body, func(v *types.Var, _ bool) {
					updatedLocals[v] = true
				})
				break
			}
		}
		logf("multiple-assignment vars: %v", updatedLocals)
		assign1 = func(v *types.Var) bool { return !updatedLocals[v] }
	}

	// import map, initially populated with caller imports.
	//
	// For simplicity we ignore existing dot imports, so that a
	// qualified identifier (QI) in the callee is always
	// represented by a QI in the caller, allowing us to treat a
	// QI like a selection on a package name.
	importMap := make(map[string][]string) // maps package path to local name(s)
	for _, imp := range caller.File.Imports {
		if pkgname, ok := importedPkgName(caller.Info, imp); ok &&
			pkgname.Name() != "." &&
			pkgname.Name() != "_" {
			path := pkgname.Imported().Path()
			importMap[path] = append(importMap[path], pkgname.Name())
		}
	}

	// localImportName returns the local name for a given imported package path.
	var newImports []*ast.ImportSpec
	localImportName := func(path string) string {
		// Does an import exist?
		for _, name := range importMap[path] {
			// Check that either the import preexisted,
			// or that it was newly added (no PkgName) but is not shadowed.
			found := caller.lookup(name)
			if is[*types.PkgName](found) || found == nil {
				return name
			}
		}

		// import added by callee
		//
		// Choose local PkgName based on last segment of
		// package path plus, if needed, a numeric suffix to
		// ensure uniqueness.
		//
		// TODO(adonovan): preserve the PkgName used
		// in the original source, or, for a dot import,
		// use the package's declared name.
		base := pathpkg.Base(path)
		name := base
		for n := 0; caller.lookup(name) != nil; n++ {
			name = fmt.Sprintf("%s%d", base, n)
		}

		// TODO(adonovan): don't use a renaming import
		// unless the local name differs from either
		// the package name or the last segment of path.
		// This requires that we tabulate (path, declared name, local name)
		// triples for each package referenced by the callee.
		logf("adding import %s %q", name, path)
		newImports = append(newImports, &ast.ImportSpec{
			Name: makeIdent(name),
			Path: &ast.BasicLit{
				Kind:  token.STRING,
				Value: strconv.Quote(path),
			},
		})
		importMap[path] = append(importMap[path], name)
		return name
	}

	// Compute the renaming of the callee's free identifiers.
	objRenames := make([]ast.Expr, len(callee.FreeObjs)) // nil => no change
	for i, obj := range callee.FreeObjs {
		// obj is a free object of the callee.
		//
		// Possible cases are:
		// - builtin function, type, or value (e.g. nil, zero)
		//   => check not shadowed in caller.
		// - package-level var/func/const/types
		//   => same package: check not shadowed in caller.
		//   => otherwise: import other package form a qualified identifier.
		//      (Unexported cross-package references were rejected already.)
		// - type parameter
		//   => not yet supported
		// - pkgname
		//   => import other package and use its local name.
		//
		// There can be no free references to labels, fields, or methods.

		var newName ast.Expr
		if obj.Kind == "pkgname" {
			// Use locally appropriate import, creating as needed.
			newName = makeIdent(localImportName(obj.PkgPath)) // imported package

		} else if !obj.ValidPos {
			// Built-in function, type, or value (e.g. nil, zero):
			// check not shadowed at caller.
			found := caller.lookup(obj.Name) // always finds something
			if found.Pos().IsValid() {
				return nil, fmt.Errorf("cannot inline because built-in %q is shadowed in caller by a %s (line %d)",
					obj.Name, objectKind(found),
					caller.Fset.PositionFor(found.Pos(), false).Line)
			}

		} else {
			// Must be reference to package-level var/func/const/type,
			// since type parameters are not yet supported.
			qualify := false
			if obj.PkgPath == callee.PkgPath {
				// reference within callee package
				if samePkg {
					// Caller and callee are in same package.
					// Check caller has not shadowed the decl.
					found := caller.lookup(obj.Name) // can't fail
					if !isPkgLevel(found) {
						return nil, fmt.Errorf("cannot inline because %q is shadowed in caller by a %s (line %d)",
							obj.Name, objectKind(found),
							caller.Fset.PositionFor(found.Pos(), false).Line)
					}
				} else {
					// Cross-package reference.
					qualify = true
				}
			} else {
				// Reference to a package-level declaration
				// in another package, without a qualified identifier:
				// it must be a dot import.
				qualify = true
			}

			// Form a qualified identifier, pkg.Name.
			if qualify {
				pkgName := localImportName(obj.PkgPath)
				newName = &ast.SelectorExpr{
					X:   makeIdent(pkgName),
					Sel: makeIdent(obj.Name),
				}
			}
		}
		objRenames[i] = newName
	}

	res := &result{
		newImports: newImports,
	}

	// Parse callee function declaration.
	calleeFset, calleeDecl, err := parseCompact(callee.Content)
	if err != nil {
		return nil, err // "can't happen"
	}

	// replaceCalleeID replaces an identifier in the callee.
	// The replacement tree must not belong to the caller; use cloneNode as needed.
	replaceCalleeID := func(offset int, repl ast.Expr) {
		id := findIdent(calleeDecl, calleeDecl.Pos()+token.Pos(offset))
		logf("- replace id %q @ #%d to %q", id.Name, offset, debugFormatNode(calleeFset, repl))
		replaceNode(calleeDecl, id, repl)
	}

	// Generate replacements for each free identifier.
	// (The same tree may be spliced in multiple times, resulting in a DAG.)
	for _, ref := range callee.FreeRefs {
		if repl := objRenames[ref.Object]; repl != nil {
			replaceCalleeID(ref.Offset, repl)
		}
	}

	// Gather the effective call arguments, including the receiver.
	// Later, elements will be eliminated (=> nil) by parameter substitution.
	args, err := arguments(caller, calleeDecl, assign1)
	if err != nil {
		return nil, err // e.g. implicit field selection cannot be made explicit
	}

	// Gather effective parameter tuple, including the receiver if any.
	// Simplify variadic parameters to slices (in all cases but one).
	var params []*parameter // including receiver; nil => parameter substituted
	{
		sig := calleeSymbol.Type().(*types.Signature)
		if sig.Recv() != nil {
			params = append(params, &parameter{
				obj:  sig.Recv(),
				info: callee.Params[0],
			})
		}
		for i := 0; i < sig.Params().Len(); i++ {
			params = append(params, &parameter{
				obj:  sig.Params().At(i),
				info: callee.Params[len(params)],
			})
		}

		// Variadic function?
		//
		// There are three possible types of call:
		// - ordinary f(a1, ..., aN)
		// - ellipsis f(a1, ..., slice...)
		// - spread   f(recv?, g()) where g() is a tuple.
		// The first two are desugared to non-variadic calls
		// with an ordinary slice parameter;
		// the third is tricky and cannot be reduced, and (if
		// a receiver is present) cannot even be literalized.
		// Fortunately it is vanishingly rare.
		//
		// TODO(adonovan): extract this to a function.
		if sig.Variadic() {
			lastParam := last(params)
			if len(args) > 0 && last(args).spread {
				// spread call to variadic: tricky
				lastParam.variadic = true
			} else {
				// ordinary/ellipsis call to variadic

				// simplify decl: func(T...) -> func([]T)
				lastParamField := last(calleeDecl.Type.Params.List)
				lastParamField.Type = &ast.ArrayType{
					Elt: lastParamField.Type.(*ast.Ellipsis).Elt,
				}

				if caller.Call.Ellipsis.IsValid() {
					// ellipsis call: f(slice...) -> f(slice)
					// nop
				} else {
					// ordinary call: f(a1, ... aN) -> f([]T{a1, ..., aN})
					n := len(params) - 1
					ordinary, extra := args[:n], args[n:]
					var elts []ast.Expr
					pure, effects := true, false
					for _, arg := range extra {
						elts = append(elts, arg.expr)
						pure = pure && arg.pure
						effects = effects || arg.effects
					}
					args = append(ordinary, &argument{
						expr: &ast.CompositeLit{
							Type: lastParamField.Type,
							Elts: elts,
						},
						typ:        lastParam.obj.Type(),
						pure:       pure,
						effects:    effects,
						duplicable: false,
						freevars:   nil, // not needed
					})
				}
			}
		}
	}

	// Note: computation below should be expressed in terms of
	// the args and params slices, not the raw material.

	// Perform parameter substitution.
	// May eliminate some elements of params/args.
	substitute(logf, caller, params, args, replaceCalleeID)

	// Update the callee's signature syntax.
	updateCalleeParams(calleeDecl, params)

	// Create a var (param = arg; ...) decl for use by some strategies.
	bindingDeclStmt := createBindingDecl(logf, caller, args, calleeDecl)

	var remainingArgs []ast.Expr
	for _, arg := range args {
		if arg != nil {
			remainingArgs = append(remainingArgs, arg.expr)
		}
	}

	// -- let the inlining strategies begin --
	//
	// When we commit to a strategy, we log a message of the form:
	//
	//   "strategy: reduce expr-context call to { return expr }"
	//
	// This is a terse way of saying:
	//
	//    we plan to reduce a call
	//    that appears in expression context
	//    to a function whose body is of the form { return expr }

	// TODO(adonovan): split this huge function into a sequence of
	// function calls with an error sentinel that means "try the
	// next strategy", and make sure each strategy writes to the
	// log the reason it didn't match.

	// Special case: eliminate a call to a function whose body is empty.
	// (=> callee has no results and caller is a statement.)
	//
	//    func f(params) {}
	//    f(args)
	//    => _, _ = args
	//
	if len(calleeDecl.Body.List) == 0 {
		logf("strategy: reduce call to empty body")

		// Evaluate the arguments for effects and delete the call entirely.
		stmt := callStmt(caller.path) // cannot fail
		res.old = stmt
		if nargs := len(remainingArgs); nargs > 0 {
			// Emit "_, _ = args" to discard results.
			// Make correction for spread calls
			// f(g()) or x.f(g()) where g() is a tuple.
			//
			// TODO(adonovan): fix: it's not valid for a
			// single AssignStmt to discard a receiver and
			// a spread argument; use a var decl with two specs.
			//
			// TODO(adonovan): if args is the []T{a1, ..., an}
			// literal synthesized during variadic simplification,
			// consider unwrapping it to its (pure) elements.
			// Perhaps there's no harm doing this for any slice literal.
			if last := last(args); last != nil {
				if tuple, ok := last.typ.(*types.Tuple); ok {
					nargs += tuple.Len() - 1
				}
			}
			res.new = &ast.AssignStmt{
				Lhs: blanks(nargs),
				Tok: token.ASSIGN,
				Rhs: remainingArgs,
			}
		} else {
			// No remaining arguments: delete call statement entirely
			res.new = &ast.EmptyStmt{}
		}
		return res, nil
	}

	// Attempt to reduce parameterless calls
	// whose result variables do not escape.
	allParamsSubstituted := forall(params, func(i int, p *parameter) bool {
		return p == nil
	})
	noResultEscapes := !exists(callee.Results, func(i int, r *paramInfo) bool {
		return r.Escapes
	})

	// Special case: call to { return exprs }.
	//
	// Reduces to:
	//	    { var (bindings); _, _ = exprs }
	//     or   _, _ = exprs
	//     or   expr
	//
	// If:
	// - the body is just "return expr" with trivial implicit conversions,
	// - all parameters can be eliminated
	//   (by substitution, or a binding decl),
	// - no result var escapes,
	// then the call expression can be replaced by the
	// callee's body expression, suitably substituted.
	if callee.BodyIsReturnExpr {
		results := calleeDecl.Body.List[0].(*ast.ReturnStmt).Results

		context := callContext(caller.path)

		// statement context
		if stmt, ok := context.(*ast.ExprStmt); ok &&
			(allParamsSubstituted && noResultEscapes || bindingDeclStmt != nil) {
			logf("strategy: reduce stmt-context call to { return exprs }")
			clearPositions(calleeDecl.Body)

			if callee.ValidForCallStmt {
				logf("callee body is valid as statement")
				// Inv: len(results) == 1
				if allParamsSubstituted && noResultEscapes {
					// Reduces to: expr
					res.old = caller.Call
					res.new = results[0]
				} else {
					// Reduces to: { var (bindings); expr }
					res.old = stmt
					res.new = &ast.BlockStmt{
						List: []ast.Stmt{
							bindingDeclStmt,
							&ast.ExprStmt{X: results[0]},
						},
					}
				}
			} else {
				logf("callee body is not valid as statement")
				// The call is a standalone statement, but the
				// callee body is not suitable as a standalone statement
				// (f() or <-ch), explicitly discard the results:
				// Reduces to: _, _ = exprs
				discard := &ast.AssignStmt{
					Lhs: blanks(callee.NumResults),
					Tok: token.ASSIGN,
					Rhs: results,
				}
				res.old = stmt
				if allParamsSubstituted && noResultEscapes {
					// Reduces to: _, _ = exprs
					res.new = discard
				} else {
					// Reduces to: { var (bindings); _, _ = exprs }
					res.new = &ast.BlockStmt{
						List: []ast.Stmt{
							bindingDeclStmt,
							discard,
						},
					}
				}
			}
			return res, nil
		}

		// expression context
		if allParamsSubstituted && noResultEscapes {
			clearPositions(calleeDecl.Body)

			if callee.NumResults == 1 {
				logf("strategy: reduce expr-context call to { return expr }")

				// A single return operand inlined to a unary
				// expression context may need parens. Otherwise:
				//    func two() int { return 1+1 }
				//    print(-two())  =>  print(-1+1) // oops!
				//
				// TODO(adonovan): do better by analyzing 'context'
				// to see whether ambiguity is possible.
				// For example, if the context is x[y:z], then
				// the x subtree is subject to precedence ambiguity
				// (replacing x by p+q would give p+q[y:z] which is wrong)
				// but the y and z subtrees are safe.
				res.old = caller.Call
				res.new = &ast.ParenExpr{X: results[0]}
			} else {
				logf("strategy: reduce spread-context call to { return expr }")

				// The call returns multiple results but is
				// not a standalone call statement. It must
				// be the RHS of a spread assignment:
				//   var x, y  = f()
				//       x, y := f()
				//       x, y  = f()
				// or the sole argument to a spread call:
				//        printf(f())
				res.old = context
				switch context := context.(type) {
				case *ast.AssignStmt:
					// Inv: the call is in Rhs[0], not Lhs.
					assign := shallowCopy(context)
					assign.Rhs = results
					res.new = assign
				case *ast.ValueSpec:
					// Inv: the call is in Values[0], not Names.
					spec := shallowCopy(context)
					spec.Values = results
					res.new = spec
				case *ast.CallExpr:
					// Inv: the Call is Args[0], not Fun.
					call := shallowCopy(context)
					call.Args = results
					res.new = call
				default:
					return nil, fmt.Errorf("internal error: unexpected context %T for spread call", context)
				}
			}
			return res, nil
		}
	}

	// Special case: tail-call.
	//
	// Inlining:
	//         return f(args)
	// where:
	//         func f(params) (results) { body }
	// reduces to:
	//         { var (bindings); body }
	//         { body }
	// so long as:
	// - all parameters can be eliminated
	//   (by substitution, or a binding decl),
	// - call is a tail-call;
	// - all returns in body have trivial result conversions;
	// - there is no label conflict;
	// - no result variable is referenced by name.
	//
	// The body may use defer, arbitrary control flow, and
	// multiple returns.
	//
	// TODO(adonovan): omit the braces if the sets of
	// names in the two blocks are disjoint.
	//
	// TODO(adonovan): add a strategy for a 'void tail
	// call', i.e. a call statement prior to an (explicit
	// or implicit) return.
	if ret, ok := callContext(caller.path).(*ast.ReturnStmt); ok &&
		len(ret.Results) == 1 &&
		callee.TrivialReturns == callee.TotalReturns &&
		(allParamsSubstituted && noResultEscapes || bindingDeclStmt != nil) &&
		!hasLabelConflict(caller.path, callee.Labels) &&
		forall(callee.Results, func(i int, p *paramInfo) bool {
			// all result vars are unreferenced
			return len(p.Refs) == 0
		}) {
		logf("strategy: reduce tail-call")
		body := calleeDecl.Body
		clearPositions(body)
		if !(allParamsSubstituted && noResultEscapes) {
			body.List = prepend(bindingDeclStmt, body.List...)
		}
		res.old = ret
		res.new = body
		return res, nil
	}

	// Special case: call to void function
	//
	// Inlining:
	//         f(args)
	// where:
	//	   func f(params) { stmts }
	// reduces to:
	//         { var (bindings); stmts }
	//         { stmts }
	// so long as:
	// - callee is a void function (no returns)
	// - callee does not use defer
	// - there is no label conflict between caller and callee
	// - all parameters can be eliminated
	//   (by substitution, or a binding decl),
	//
	// If there is only a single statement, the braces are omitted.
	if stmt := callStmt(caller.path); stmt != nil &&
		(allParamsSubstituted && noResultEscapes || bindingDeclStmt != nil) &&
		!callee.HasDefer &&
		!hasLabelConflict(caller.path, callee.Labels) &&
		callee.TotalReturns == 0 {
		logf("strategy: reduce stmt-context call to { stmts }")
		body := calleeDecl.Body
		var repl ast.Stmt = body
		clearPositions(repl)
		if !(allParamsSubstituted && noResultEscapes) {
			body.List = prepend(bindingDeclStmt, body.List...)
		}
		if len(body.List) == 1 {
			repl = body.List[0] // singleton: omit braces
		}
		res.old = stmt
		res.new = repl
		return res, nil
	}

	// TODO(adonovan): parameterless call to { stmt; return expr }
	// from one of these contexts:
	//    x, y     = f()
	//    x, y    := f()
	//    var x, y = f()
	// =>
	//    var (x T1, y T2); { stmts; x, y = expr }
	//
	// Because the params are no longer declared simultaneously
	// we need to check that (for example) x ∉ freevars(T2),
	// in addition to the usual checks for arg/result conversions,
	// complex control, etc.
	// Also test cases where expr is an n-ary call (spread returns).

	// Literalization isn't quite infallible.
	// Consider a spread call to a method in which
	// no parameters are eliminated, e.g.
	// 	new(T).f(g())
	// where
	//  	func (recv *T) f(x, y int) { body }
	//  	func g() (int, int)
	// This would be literalized to:
	// 	func (recv *T, x, y int) { body }(new(T), g()),
	// which is not a valid argument list because g() must appear alone.
	// Reject this case for now.
	if len(args) == 2 && args[0] != nil && args[1] != nil && is[*types.Tuple](args[1].typ) {
		return nil, fmt.Errorf("can't yet inline spread call to method")
	}

	// Infallible general case: literalization.
	logf("strategy: literalization")

	// Emit a new call to a function literal in place of
	// the callee name, with appropriate replacements.
	newCall := &ast.CallExpr{
		Fun: &ast.FuncLit{
			Type: calleeDecl.Type,
			Body: calleeDecl.Body,
		},
		Ellipsis: token.NoPos, // f(slice...) is always simplified
		Args:     remainingArgs,
	}
	clearPositions(newCall.Fun)
	res.old = caller.Call
	res.new = newCall
	return res, nil
}

type argument struct {
	expr       ast.Expr
	typ        types.Type      // may be tuple for sole non-receiver arg in spread call
	spread     bool            // final arg is call() assigned to multiple params
	pure       bool            // expr is pure (doesn't read variables)
	effects    bool            // expr has effects (updates variables)
	duplicable bool            // expr may be duplicated
	freevars   map[string]bool // free names of expr
}

// arguments returns the effective arguments of the call.
//
// If the receiver argument and parameter have
// different pointerness, make the "&" or "*" explicit.
//
// Also, if x.f() is shorthand for promoted method x.y.f(),
// make the .y explicit in T.f(x.y, ...).
//
// Beware that:
//
//   - a method can only be called through a selection, but only
//     the first of these two forms needs special treatment:
//
//     expr.f(args)     -> ([&*]expr, args)	MethodVal
//     T.f(recv, args)  -> (    expr, args)	MethodExpr
//
//   - the presence of a value in receiver-position in the call
//     is a property of the caller, not the callee. A method
//     (calleeDecl.Recv != nil) may be called like an ordinary
//     function.
//
//   - the types.Signatures seen by the caller (from
//     StaticCallee) and by the callee (from decl type)
//     differ in this case.
//
// In a spread call f(g()), the sole ordinary argument g(),
// always last in args, has a tuple type.
//
// We compute type-based predicates like pure, duplicable,
// freevars, etc, now, before we start modifying syntax.
func arguments(caller *Caller, calleeDecl *ast.FuncDecl, assign1 func(*types.Var) bool) ([]*argument, error) {
	var args []*argument

	callArgs := caller.Call.Args
	if calleeDecl.Recv != nil {
		sel := astutil.Unparen(caller.Call.Fun).(*ast.SelectorExpr)
		seln := caller.Info.Selections[sel]
		var recvArg ast.Expr
		switch seln.Kind() {
		case types.MethodVal: // recv.f(callArgs)
			recvArg = sel.X
		case types.MethodExpr: // T.f(recv, callArgs)
			recvArg = callArgs[0]
			callArgs = callArgs[1:]
		}
		if recvArg != nil {
			// Compute all the type-based predicates now,
			// before we start meddling with the syntax;
			// the meddling will update them.
			arg := &argument{
				expr:       recvArg,
				typ:        caller.Info.TypeOf(recvArg),
				pure:       pure(caller.Info, assign1, recvArg),
				effects:    effects(caller.Info, recvArg),
				duplicable: duplicable(caller.Info, recvArg),
				freevars:   freeVars(caller.Info, recvArg),
			}
			recvArg = nil // prevent accidental use

			// Move receiver argument recv.f(args) to argument list f(&recv, args).
			args = append(args, arg)

			// Make field selections explicit (recv.f -> recv.y.f),
			// updating arg.{expr,typ}.
			indices := seln.Index()
			for _, index := range indices[:len(indices)-1] {
				t := deref(arg.typ)
				fld := typeparams.CoreType(t).(*types.Struct).Field(index)
				if fld.Pkg() != caller.Types && !fld.Exported() {
					return nil, fmt.Errorf("in %s, implicit reference to unexported field .%s cannot be made explicit",
						debugFormatNode(caller.Fset, caller.Call.Fun),
						fld.Name())
				}
				arg.expr = &ast.SelectorExpr{
					X:   arg.expr,
					Sel: makeIdent(fld.Name()),
				}
				arg.typ = fld.Type()
			}

			// Make * or & explicit.
			argIsPtr := arg.typ != deref(arg.typ)
			paramIsPtr := is[*types.Pointer](seln.Obj().Type().(*types.Signature).Recv().Type())
			if !argIsPtr && paramIsPtr {
				// &recv
				arg.expr = &ast.UnaryExpr{Op: token.AND, X: arg.expr}
				arg.typ = types.NewPointer(arg.typ)
			} else if argIsPtr && !paramIsPtr {
				// *recv
				arg.expr = &ast.StarExpr{X: arg.expr}
				arg.typ = deref(arg.typ)

				// Technically *recv is non-pure and
				// non-duplicable, as side effects
				// could change the pointer between
				// multiple reads. But unfortunately
				// this really degrades many of our tests.
				// (The indices loop above should similarly
				// update these flags when traversing pointers.)
				//
				// TODO(adonovan): improve the precision of
				// purity and duplicability.
				// For example, *new(T) is actually pure.
				// And *ptr, where ptr doesn't escape and
				// has no assignments other than its decl,
				// is also pure; this is very common.
				//
				// arg.pure = false
			}
		}
	}
	for _, expr := range callArgs {
		typ := caller.Info.TypeOf(expr)
		args = append(args, &argument{
			expr:       expr,
			typ:        typ,
			spread:     is[*types.Tuple](typ), // => last
			pure:       pure(caller.Info, assign1, expr),
			effects:    effects(caller.Info, expr),
			duplicable: duplicable(caller.Info, expr),
			freevars:   freeVars(caller.Info, expr),
		})
	}
	return args, nil
}

type parameter struct {
	obj      *types.Var // parameter var from caller's signature
	info     *paramInfo // information from AnalyzeCallee
	variadic bool       // (final) parameter is unsimplified ...T
}

// substitute implements parameter elimination by substitution.
//
// It considers each parameter and its corresponding argument in turn
// and evaluate these conditions:
//
//   - the parameter is neither address-taken nor assigned;
//   - the argument is pure;
//   - if the parameter refcount is zero, the argument must
//     not contain the last use of a local var;
//   - if the parameter refcount is > 1, the argument must be duplicable;
//   - the argument (or types.Default(argument) if it's untyped) has
//     the same type as the parameter.
//
// If all conditions are met then the parameter can be substituted and
// each reference to it replaced by the argument. In that case, the
// replaceCalleeID function is called for each reference to the
// parameter, and is provided with its relative offset and replacement
// expression (argument), and the corresponding elements of params and
// args are replaced by nil.
func substitute(logf func(string, ...any), caller *Caller, params []*parameter, args []*argument, replaceCalleeID func(offset int, repl ast.Expr)) {
	// Inv:
	//  in        calls to     variadic, len(args) >= len(params)-1
	//  in spread calls to non-variadic, len(args) <  len(params)
	//  in spread calls to     variadic, len(args) <= len(params)
	// (In spread calls len(args) = 1, or 2 if call has receiver.)
	// Non-spread variadics have been simplified away already,
	// so the args[i] lookup is safe if we stop after the spread arg.
next:
	for i, param := range params {
		arg := args[i]
		// Check argument against parameter.
		//
		// Beware: don't use types.Info on arg since
		// the syntax may be synthetic (not created by parser)
		// and thus lacking positions and types;
		// do it earlier (see pure/duplicable/freevars).

		if arg.spread {
			logf("keeping param %q and following ones: argument %s is spread",
				param.info.Name, debugFormatNode(caller.Fset, arg.expr))
			break // spread => last argument, but not always last parameter
		}
		assert(!param.variadic, "unsimplified variadic parameter")
		if param.info.Escapes {
			logf("keeping param %q: escapes from callee", param.info.Name)
			continue
		}
		if param.info.Assigned {
			logf("keeping param %q: assigned by callee", param.info.Name)
			continue // callee needs the parameter variable
		}
		if !arg.pure || arg.effects {
			// TODO(adonovan): conduct a deeper analysis of callee effects
			// to relax this constraint.
			logf("keeping param %q: argument %s is impure",
				param.info.Name, debugFormatNode(caller.Fset, arg.expr))
			continue // unsafe to change order or cardinality of effects
		}
		if len(param.info.Refs) > 1 && !arg.duplicable {
			logf("keeping param %q: argument is not duplicable", param.info.Name)
			continue // incorrect or poor style to duplicate an expression
		}
		if len(param.info.Refs) == 0 {
			// Eliminating an unreferenced parameter might
			// remove the last reference to a caller local var.
			for free := range arg.freevars {
				if v, ok := caller.lookup(free).(*types.Var); ok {
					// TODO(adonovan): be more precise and check
					// that v is defined within the body of the caller
					// function (if any) and is indeed referenced
					// only by the call. (See assign1 for analysis
					// of enclosing func.)
					logf("keeping param %q: arg contains perhaps the last reference to possible caller local %v @ %v",
						param.info.Name, v, caller.Fset.PositionFor(v.Pos(), false))
					continue next
				}
			}
		}

		// Check that eliminating the parameter wouldn't materially
		// change the type.
		//
		// (We don't simply wrap the argument in an explicit conversion
		// to the parameter type because that could increase allocation
		// in the number of (e.g.) string -> any conversions.
		// Even when Uses = 1, the sole ref might be in a loop or lambda that
		// is multiply executed.)
		if len(param.info.Refs) > 0 && !trivialConversion(args[i].typ, params[i].obj) {
			logf("keeping param %q: argument passing converts %s to type %s",
				param.info.Name, args[i].typ, params[i].obj.Type())
			continue // implicit conversion is significant
		}

		// Check for shadowing.
		//
		// Consider inlining a call f(z, 1) to
		// func f(x, y int) int { z := y; return x + y + z }:
		// we can't replace x in the body by z (or any
		// expression that has z as a free identifier)
		// because there's an intervening declaration of z
		// that would shadow the caller's one.
		for free := range arg.freevars {
			if param.info.Shadow[free] {
				logf("keeping param %q: cannot replace with argument as it has free ref to %s that is shadowed", param.info.Name, free)
				continue next // shadowing conflict
			}
		}

		// It is safe to eliminate param and replace it with arg.
		// No additional parens are required around arg for
		// the supported "pure" expressions.
		//
		// Because arg.expr belongs to the caller,
		// we clone it before splicing it into the callee tree.
		logf("replacing parameter %q by argument %q",
			param.info.Name, debugFormatNode(caller.Fset, arg.expr))
		for _, ref := range param.info.Refs {
			replaceCalleeID(ref, cloneNode(arg.expr).(ast.Expr))
		}
		params[i] = nil // substituted
		args[i] = nil   // substituted
	}
}

// updateCalleeParams updates the calleeDecl syntax to remove
// substituted parameters and move the receiver (if any) to the head
// of the ordinary parameters.
func updateCalleeParams(calleeDecl *ast.FuncDecl, params []*parameter) {
	// The logic is fiddly because of the three forms of ast.Field:
	//
	//	func(int), func(x int), func(x, y int)
	//
	// Also, ensure that all remaining parameters are named
	// to avoid a mix of named/unnamed when joining (recv, params...).
	// func (T) f(int, bool) -> (_ T, _ int, _ bool)
	// (Strictly, we need do this only for methods and only when
	// the namednesses of Recv and Params differ; that might be tidier.)

	paramIdx := 0 // index in original parameter list (incl. receiver)
	var newParams []*ast.Field
	filterParams := func(field *ast.Field) {
		var names []*ast.Ident
		if field.Names == nil {
			// Unnamed parameter field (e.g. func f(int)
			if params[paramIdx] != nil {
				// Give it an explicit name "_" since we will
				// make the receiver (if any) a regular parameter
				// and one cannot mix named and unnamed parameters.
				names = append(names, makeIdent("_"))
			}
			paramIdx++
		} else {
			// Named parameter field e.g. func f(x, y int)
			// Remove substituted parameters in place.
			// If all were substituted, delete field.
			for _, id := range field.Names {
				if pinfo := params[paramIdx]; pinfo != nil {
					// Rename unreferenced parameters with "_".
					// This is crucial for binding decls, since
					// unlike parameters, they are subject to
					// "unreferenced var" checks.
					if len(pinfo.info.Refs) == 0 {
						id = makeIdent("_")
					}
					names = append(names, id)
				}
				paramIdx++
			}
		}
		if names != nil {
			newParams = append(newParams, &ast.Field{
				Names: names,
				Type:  field.Type,
			})
		}
	}
	if calleeDecl.Recv != nil {
		filterParams(calleeDecl.Recv.List[0])
		calleeDecl.Recv = nil
	}
	for _, field := range calleeDecl.Type.Params.List {
		filterParams(field)
	}
	calleeDecl.Type.Params.List = newParams
}

// createBindingDecl constructs a "binding decl" that implements
// parameter assignment.
//
// If we succeed, the declaration may be used by reduction
// strategies to relax the requirement that all parameters
// have been substituted.
//
// For example, a call:
//
//	f(a0, a1, a2)
//
// where:
//
//	func f(p0, p1 T0, p2 T1) { body }
//
// reduces to:
//
//	{
//	  var (
//	    p0, p1 T0 = a0, a1
//	    p2     T1 = a2
//	  )
//	  body
//	}
//
// so long as p0, p1 ∉ freevars(T1) or freevars(a2), and so on,
// because each spec is statically resolved in sequence and
// dynamically assigned in sequence. By contrast, all
// parameters are resolved simultaneously and assigned
// simultaneously.
//
// The pX names should already be blank ("_") if the parameter
// is unreferenced; this avoids "unreferenced local var" checks.
//
// Strategies may impose additional checks on return
// conversions, labels, defer, etc.
func createBindingDecl(logf func(string, ...any), caller *Caller, args []*argument, calleeDecl *ast.FuncDecl) ast.Stmt {
	// Spread calls are tricky as they may not align with the
	// parameters' field groupings nor types.
	// For example, given
	//   func g() (int, string)
	// the call
	//   f(g())
	// is legal with these decls of f:
	//   func f(int, string)
	//   func f(x, y any)
	//   func f(x, y ...any)
	// TODO(adonovan): support binding decls for spread calls by
	// splitting parameter groupings as needed.
	if lastArg := last(args); lastArg != nil && lastArg.spread {
		logf("binding decls not yet supported for spread calls")
		return nil
	}

	// Compute remaining argument expressions.
	var values []ast.Expr
	for _, arg := range args {
		if arg != nil {
			values = append(values, arg.expr)
		}
	}

	var (
		specs    []ast.Spec
		shadowed = make(map[string]bool) // names defined by previous specs
	)
	for _, field := range calleeDecl.Type.Params.List {
		// Each field (param group) becomes a ValueSpec.
		spec := &ast.ValueSpec{
			Names:  field.Names,
			Type:   field.Type,
			Values: values[:len(field.Names)],
		}
		values = values[len(field.Names):]

		// Compute union of free names of type and values
		// and detect shadowing. Values is the arguments
		// (caller syntax), so we can use type info.
		// But Type is the untyped callee syntax,
		// so we have to use a syntax-only algorithm.
		free := make(map[string]bool)
		for _, value := range spec.Values {
			for name := range freeVars(caller.Info, value) {
				free[name] = true
			}
		}
		freeishNames(free, field.Type)
		for name := range free {
			if shadowed[name] {
				logf("binding decl would shadow free name %q", name)
				return nil
			}
		}
		for _, id := range spec.Names {
			if id.Name != "_" {
				shadowed[id.Name] = true
			}
		}

		specs = append(specs, spec)
	}
	assert(len(values) == 0, "args/params mismatch")
	decl := &ast.DeclStmt{
		Decl: &ast.GenDecl{
			Tok:   token.VAR,
			Specs: specs,
		},
	}
	logf("binding decl: %s", debugFormatNode(caller.Fset, decl))
	return decl
}

// lookup does a symbol lookup in the lexical environment of the caller.
func (caller *Caller) lookup(name string) types.Object {
	pos := caller.Call.Pos()
	for _, n := range caller.path {
		// The function body scope (containing not just params)
		// is associated with FuncDecl.Type, not FuncDecl.Body.
		if decl, ok := n.(*ast.FuncDecl); ok {
			n = decl.Type
		}
		if scope := caller.Info.Scopes[n]; scope != nil {
			if _, obj := scope.LookupParent(name, pos); obj != nil {
				return obj
			}
		}
	}
	return nil
}

// -- predicates over expressions --

// freeVars returns the names of all free identifiers of e:
// those lexically referenced by it but not defined within it.
// (Fields and methods are not included.)
func freeVars(info *types.Info, e ast.Expr) map[string]bool {
	free := make(map[string]bool)
	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			// The isField check is so that we don't treat T{f: 0} as a ref to f.
			if obj, ok := info.Uses[id]; ok && !within(obj.Pos(), e) && !isField(obj) {
				free[obj.Name()] = true
			}
		}
		return true
	})
	return free
}

// freeishNames computes an over-approximation to the free names
// of the type syntax t, inserting values into the map.
//
// Because we don't have go/types annotations, we can't give an exact
// result in all cases. In particular, an array type [n]T might have a
// size such as unsafe.Sizeof(func() int{stmts...}()) and now the
// precise answer depends upon all the statement syntax too. But that
// never happens in practice.
func freeishNames(free map[string]bool, t ast.Expr) {
	var visit func(n ast.Node) bool
	visit = func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Ident:
			free[n.Name] = true

		case *ast.SelectorExpr:
			ast.Inspect(n.X, visit)
			return false // don't visit .Sel

		case *ast.Field:
			ast.Inspect(n.Type, visit)
			// Don't visit .Names:
			// FuncType parameters, interface methods, struct fields
			return false
		}
		return true
	}
	ast.Inspect(t, visit)
}

// effects reports whether an expression might change the state of the
// program (through function calls and channel receives) and affect
// the evaluation of subsequent expressions.
func effects(info *types.Info, expr ast.Expr) bool {
	effects := false
	ast.Inspect(expr, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false // prune descent

		case *ast.CallExpr:
			if !info.Types[n.Fun].IsType() {
				// A conversion T(x) has only the effect of its operand.
			} else if !callsPureBuiltin(info, n) {
				// A handful of built-ins have no effect
				// beyond those of their arguments.
				// All other calls (including append, copy, recover)
				// have unknown effects.
				effects = true
			}

		case *ast.UnaryExpr:
			if n.Op == token.ARROW { // <-ch
				effects = true
			}
		}
		return true
	})
	return effects
}

// pure reports whether an expression has the same result no matter
// when it is executed relative to other expressions, so it can be
// commuted with any other expression or statement without changing
// its meaning.
//
// An expression is considered impure if it reads the contents of any
// variable, with the exception of "single assignment" local variables
// (as classified by the provided callback), which are never updated
// after their initialization.
//
// Pure does not imply duplicable: for example, new(T) and T{} are
// pure expressions but both return a different value each time they
// are evaluated, so they are not safe to duplicate.
//
// Purity does not imply freedom from run-time panics. We assume that
// target programs do not encounter run-time panics nor depend on them
// for correct operation.
//
// TODO(adonovan): add unit tests of this function.
func pure(info *types.Info, assign1 func(*types.Var) bool, e ast.Expr) bool {
	var pure func(e ast.Expr) bool
	pure = func(e ast.Expr) bool {
		switch e := e.(type) {
		case *ast.ParenExpr:
			return pure(e.X)

		case *ast.Ident:
			if v, ok := info.Uses[e].(*types.Var); ok {
				// In general variables are impure
				// as they may be updated, but
				// single-assignment local variables
				// never change value.
				//
				// We assume all package-level variables
				// may be updated, but for non-exported
				// ones we could do better by analyzing
				// the complete package.
				return !isPkgLevel(v) && assign1(v)
			}

			// All other kinds of reference are pure.
			return true

		case *ast.FuncLit:
			// A function literal may allocate a closure that
			// references mutable variables, but mutation
			// cannot be observed without calling the function,
			// and calls are considered impure.
			return true

		case *ast.BasicLit:
			return true

		case *ast.UnaryExpr: // + - ! ^ & but not <-
			return e.Op != token.ARROW && pure(e.X)

		case *ast.BinaryExpr: // arithmetic, shifts, comparisons, &&/||
			return pure(e.X) && pure(e.Y)

		case *ast.CallExpr:
			// A conversion is as pure as its operand.
			if info.Types[e.Fun].IsType() {
				return pure(e.Args[0])
			}

			// Calls to some built-ins are as pure as their arguments.
			if callsPureBuiltin(info, e) {
				for _, arg := range e.Args {
					if !pure(arg) {
						return false
					}
				}
				return true
			}

			// All other calls are impure, so we can
			// reject them without even looking at e.Fun.
			//
			// More sophisticated analysis could infer purity in
			// commonly used functions such as strings.Contains;
			// perhaps we could offer the client a hook so that
			// go/analysis-based implementation could exploit the
			// results of a purity analysis. But that would make
			// the inliner's choices harder to explain.
			return false

		case *ast.CompositeLit:
			// T{...} is as pure as its elements.
			for _, elt := range e.Elts {
				if kv, ok := elt.(*ast.KeyValueExpr); ok {
					if !pure(kv.Value) {
						return false
					}
					if id, ok := kv.Key.(*ast.Ident); ok {
						if v, ok := info.Uses[id].(*types.Var); ok && v.IsField() {
							continue // struct {field: value}
						}
					}
					// map/slice/array {key: value}
					if !pure(kv.Key) {
						return false
					}

				} else if !pure(elt) {
					return false
				}
			}
			return true

		case *ast.SelectorExpr:
			if sel, ok := info.Selections[e]; ok {
				switch sel.Kind() {
				case types.MethodExpr:
					// A method expression T.f acts like a
					// reference to a func decl, so it is pure.
					return true

				case types.MethodVal:
					// A method value x.f acts like a
					// closure around a T.f(x, ...) call,
					// so it is as pure as x.
					return pure(e.X)

				case types.FieldVal:
					// A field selection x.f is pure if
					// x is pure and the selection does
					// not indirect a pointer.
					return !sel.Indirect() && pure(e.X)

				default:
					panic(sel)
				}
			} else {
				// A qualified identifier is
				// treated like an unqualified one.
				return pure(e.Sel)
			}

		case *ast.StarExpr:
			return false // *ptr depends on the state of the heap

		default:
			return false
		}
	}
	return pure(e)
}

func callsPureBuiltin(info *types.Info, call *ast.CallExpr) bool {
	if id, ok := astutil.Unparen(call.Fun).(*ast.Ident); ok {
		if b, ok := info.ObjectOf(id).(*types.Builtin); ok {
			switch b.Name() {
			case "len", "cap", "complex", "imag", "real", "make", "new", "max", "min":
				return true
			}
			// Not: append clear close copy delete panic print println recover
		}
	}
	return false
}

// duplicable reports whether it is appropriate for the expression to
// be freely duplicated.
//
// Given the declaration
//
//	func f(x T) T { return x + g() + x }
//
// an argument y is considered duplicable if we would wish to see a
// call f(y) simplified to y+g()+y. This is true for identifiers,
// integer literals, unary negation, and selectors x.f where x is not
// a pointer. But we would not wish to duplicate expressions that:
// - have side effects (e.g. nearly all calls),
// - are not referentially transparent (e.g. &T{}, ptr.field), or
// - are long (e.g. "huge string literal").
func duplicable(info *types.Info, e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.ParenExpr:
		return duplicable(info, e.X)
	case *ast.Ident:
		return true
	case *ast.BasicLit:
		return e.Kind == token.INT
	case *ast.UnaryExpr: // e.g. +1, -1
		return (e.Op == token.ADD || e.Op == token.SUB) && duplicable(info, e.X)
	case *ast.CallExpr:
		// Don't treat a conversion T(x) as duplicable even
		// if x is duplicable because it could duplicate
		// allocations. There may be cases to tease apart here.
		return false
	case *ast.SelectorExpr:
		if sel, ok := info.Selections[e]; ok {
			// A field or method selection x.f is referentially
			// transparent if it does not indirect a pointer.
			return !sel.Indirect()
		}
		// A qualified identifier pkg.Name is referentially transparent.
		return true
	default:
		return false
	}
}

// -- inline helpers --

func assert(cond bool, msg string) {
	if !cond {
		panic(msg)
	}
}

// blanks returns a slice of n > 0 blank identifiers.
func blanks(n int) []ast.Expr {
	if n == 0 {
		panic("blanks(0)")
	}
	res := make([]ast.Expr, n)
	for i := range res {
		res[i] = makeIdent("_")
	}
	return res
}

func makeIdent(name string) *ast.Ident {
	return &ast.Ident{Name: name}
}

// importedPkgName returns the PkgName object declared by an ImportSpec.
// TODO(adonovan): make this a method of types.Info (#62037).
func importedPkgName(info *types.Info, imp *ast.ImportSpec) (*types.PkgName, bool) {
	var obj types.Object
	if imp.Name != nil {
		obj = info.Defs[imp.Name]
	} else {
		obj = info.Implicits[imp]
	}
	pkgname, ok := obj.(*types.PkgName)
	return pkgname, ok
}

func isPkgLevel(obj types.Object) bool {
	// TODO(adonovan): why not simply: obj.Parent() == obj.Pkg().Scope()?
	return obj.Pkg().Scope().Lookup(obj.Name()) == obj
}

// callContext returns the node immediately enclosing the call
// (specified as a PathEnclosingInterval), ignoring parens.
func callContext(callPath []ast.Node) ast.Node {
	_ = callPath[0].(*ast.CallExpr) // sanity check
	for _, n := range callPath[1:] {
		if !is[*ast.ParenExpr](n) {
			return n
		}
	}
	return nil
}

// hasLabelConflict reports whether the set of labels of the function
// enclosing the call (specified as a PathEnclosingInterval)
// intersects with the set of callee labels.
func hasLabelConflict(callPath []ast.Node, calleeLabels []string) bool {
	var callerBody *ast.BlockStmt
	switch f := callerFunc(callPath).(type) {
	case *ast.FuncDecl:
		callerBody = f.Body
	case *ast.FuncLit:
		callerBody = f.Body
	}
	conflict := false
	if callerBody != nil {
		ast.Inspect(callerBody, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncLit:
				return false // prune traversal
			case *ast.LabeledStmt:
				for _, label := range calleeLabels {
					if label == n.Label.Name {
						conflict = true
					}
				}
			}
			return true
		})
	}
	return conflict
}

// callerFunc returns the innermost Func{Decl,Lit} node enclosing the
// call (specified as a PathEnclosingInterval).
func callerFunc(callPath []ast.Node) ast.Node {
	_ = callPath[0].(*ast.CallExpr) // sanity check
	for _, n := range callPath[1:] {
		if is[*ast.FuncDecl](n) || is[*ast.FuncLit](n) {
			return n
		}
	}
	return nil
}

// callStmt reports whether the function call (specified
// as a PathEnclosingInterval) appears within an ExprStmt,
// and returns it if so.
func callStmt(callPath []ast.Node) *ast.ExprStmt {
	stmt, _ := callContext(callPath).(*ast.ExprStmt)
	return stmt
}

// replaceNode performs a destructive update of the tree rooted at
// root, replacing each occurrence of "from" with "to". If to is nil and
// the element is within a slice, the slice element is removed.
//
// The root itself cannot be replaced; an attempt will panic.
//
// This function must not be called on the caller's syntax tree.
//
// TODO(adonovan): polish this up and move it to astutil package.
// TODO(adonovan): needs a unit test.
func replaceNode(root ast.Node, from, to ast.Node) {
	if from == nil {
		panic("from == nil")
	}
	if reflect.ValueOf(from).IsNil() {
		panic(fmt.Sprintf("from == (%T)(nil)", from))
	}
	if from == root {
		panic("from == root")
	}
	found := false
	var parent reflect.Value // parent variable of interface type, containing a pointer
	var visit func(reflect.Value)
	visit = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.Ptr:
			if v.Interface() == from {
				found = true

				// If v is a struct field or array element
				// (e.g. Field.Comment or Field.Names[i])
				// then it is addressable (a pointer variable).
				//
				// But if it was the value an interface
				// (e.g. *ast.Ident within ast.Node)
				// then it is non-addressable, and we need
				// to set the enclosing interface (parent).
				if !v.CanAddr() {
					v = parent
				}

				// to=nil => use zero value
				var toV reflect.Value
				if to != nil {
					toV = reflect.ValueOf(to)
				} else {
					toV = reflect.Zero(v.Type()) // e.g. ast.Expr(nil)
				}
				v.Set(toV)

			} else if !v.IsNil() {
				switch v.Interface().(type) {
				case *ast.Object, *ast.Scope:
					// Skip fields of types potentially involved in cycles.
				default:
					visit(v.Elem())
				}
			}

		case reflect.Struct:
			for i := 0; i < v.Type().NumField(); i++ {
				visit(v.Field(i))
			}

		case reflect.Slice:
			compact := false
			for i := 0; i < v.Len(); i++ {
				visit(v.Index(i))
				if v.Index(i).IsNil() {
					compact = true
				}
			}
			if compact {
				// Elements were deleted. Eliminate nils.
				// (Do this is a second pass to avoid
				// unnecessary writes in the common case.)
				j := 0
				for i := 0; i < v.Len(); i++ {
					if !v.Index(i).IsNil() {
						v.Index(j).Set(v.Index(i))
						j++
					}
				}
				v.SetLen(j)
			}
		case reflect.Interface:
			parent = v
			visit(v.Elem())

		case reflect.Array, reflect.Chan, reflect.Func, reflect.Map, reflect.UnsafePointer:
			panic(v) // unreachable in AST
		default:
			// bool, string, number: nop
		}
		parent = reflect.Value{}
	}
	visit(reflect.ValueOf(root))
	if !found {
		panic(fmt.Sprintf("%T not found", from))
	}
}

// cloneNode returns a deep copy of a Node.
// It omits pointers to ast.{Scope,Object} variables.
func cloneNode(n ast.Node) ast.Node {
	var clone func(x reflect.Value) reflect.Value
	set := func(dst, src reflect.Value) {
		src = clone(src)
		if src.IsValid() {
			dst.Set(src)
		}
	}
	clone = func(x reflect.Value) reflect.Value {
		switch x.Kind() {
		case reflect.Ptr:
			if x.IsNil() {
				return x
			}
			// Skip fields of types potentially involved in cycles.
			switch x.Interface().(type) {
			case *ast.Object, *ast.Scope:
				return reflect.Zero(x.Type())
			}
			y := reflect.New(x.Type().Elem())
			set(y.Elem(), x.Elem())
			return y

		case reflect.Struct:
			y := reflect.New(x.Type()).Elem()
			for i := 0; i < x.Type().NumField(); i++ {
				set(y.Field(i), x.Field(i))
			}
			return y

		case reflect.Slice:
			y := reflect.MakeSlice(x.Type(), x.Len(), x.Cap())
			for i := 0; i < x.Len(); i++ {
				set(y.Index(i), x.Index(i))
			}
			return y

		case reflect.Interface:
			y := reflect.New(x.Type()).Elem()
			set(y, x.Elem())
			return y

		case reflect.Array, reflect.Chan, reflect.Func, reflect.Map, reflect.UnsafePointer:
			panic(x) // unreachable in AST

		default:
			return x // bool, string, number
		}
	}
	return clone(reflect.ValueOf(n)).Interface().(ast.Node)
}

// clearPositions destroys token.Pos information within the tree rooted at root,
// as positions in callee trees may cause caller comments to be emitted prematurely.
//
// In general it isn't safe to clear a valid Pos because some of them
// (e.g. CallExpr.Ellipsis, TypeSpec.Assign) are significant to
// go/printer, so this function sets each non-zero Pos to 1, which
// suffices to avoid advancing the printer's comment cursor.
//
// This function mutates its argument; do not invoke on caller syntax.
//
// TODO(adonovan): remove this horrendous workaround when #20744 is finally fixed.
func clearPositions(root ast.Node) {
	posType := reflect.TypeOf(token.NoPos)
	ast.Inspect(root, func(n ast.Node) bool {
		if n != nil {
			v := reflect.ValueOf(n).Elem() // deref the pointer to struct
			fields := v.Type().NumField()
			for i := 0; i < fields; i++ {
				f := v.Field(i)
				if f.Type() == posType {
					// Clearing Pos arbitrarily is destructive,
					// as its presence may be semantically significant
					// (e.g. CallExpr.Ellipsis, TypeSpec.Assign)
					// or affect formatting preferences (e.g. GenDecl.Lparen).
					if f.Interface() != token.NoPos {
						f.Set(reflect.ValueOf(token.Pos(1)))
					}
				}
			}
		}
		return true
	})
}

// findIdent returns the Ident beneath root that has the given pos.
func findIdent(root ast.Node, pos token.Pos) *ast.Ident {
	// TODO(adonovan): opt: skip subtrees that don't contain pos.
	var found *ast.Ident
	ast.Inspect(root, func(n ast.Node) bool {
		if found != nil {
			return false
		}
		if id, ok := n.(*ast.Ident); ok {
			if id.Pos() == pos {
				found = id
			}
		}
		return true
	})
	if found == nil {
		panic(fmt.Sprintf("findIdent %d not found in %s",
			pos, debugFormatNode(token.NewFileSet(), root)))
	}
	return found
}

func prepend[T any](elem T, slice ...T) []T {
	return append([]T{elem}, slice...)
}

// debugFormatNode formats a node or returns a formatting error.
// Its sloppy treatment of errors is appropriate only for logging.
func debugFormatNode(fset *token.FileSet, n ast.Node) string {
	var out strings.Builder
	if err := format.Node(&out, fset, n); err != nil {
		out.WriteString(err.Error())
	}
	return out.String()
}

func shallowCopy[T any](ptr *T) *T {
	copy := *ptr
	return &copy
}

// ∀
func forall[T any](list []T, f func(i int, x T) bool) bool {
	for i, x := range list {
		if !f(i, x) {
			return false
		}
	}
	return true
}

// ∃
func exists[T any](list []T, f func(i int, x T) bool) bool {
	for i, x := range list {
		if f(i, x) {
			return true
		}
	}
	return false
}

// last returns the last element of a slice, or zero if empty.
func last[T any](slice []T) T {
	n := len(slice)
	if n > 0 {
		return slice[n-1]
	}
	return *new(T)
}

// canImport reports whether one package is allowed to import another.
//
// TODO(adonovan): allow customization of the accessibility relation
// (e.g. for Bazel).
func canImport(from, to string) bool {
	// TODO(adonovan): better segment hygiene.
	if strings.HasPrefix(to, "internal/") {
		// Special case: only std packages may import internal/...
		// We can't reliably know whether we're in std, so we
		// use a heuristic on the first segment.
		first, _, _ := strings.Cut(from, "/")
		if strings.Contains(first, ".") {
			return false // example.com/foo ∉ std
		}
		if first == "testdata" {
			return false // testdata/foo ∉ std
		}
	}
	if i := strings.LastIndex(to, "/internal/"); i >= 0 {
		return strings.HasPrefix(from, to[:i])
	}
	return true
}

// consistentOffsets reports whether the portion of caller.Content
// that corresponds to caller.Call can be parsed as a call expression.
// If not, the client has provided inconsistent information, possibly
// because they forgot to ignore line directives when computing the
// filename enclosing the call.
// This is just a heuristic.
func consistentOffsets(caller *Caller) bool {
	start := offsetOf(caller.Fset, caller.Call.Pos())
	end := offsetOf(caller.Fset, caller.Call.End())
	if !(0 < start && start < end && end <= len(caller.Content)) {
		return false
	}
	expr, err := parser.ParseExpr(string(caller.Content[start:end]))
	if err != nil {
		return false
	}
	return is[*ast.CallExpr](expr)
}
