package python

import (
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/repo"
	"github.com/bazelbuild/bazel-gazelle/resolve"
	"github.com/bazelbuild/bazel-gazelle/rule"
	bzl "github.com/bazelbuild/buildtools/build"
	"github.com/emirpasic/gods/sets/treeset"
	godsutils "github.com/emirpasic/gods/utils"
	"go.starlark.net/repl"
	"go.starlark.net/starlark"
	"go.starlark.net/syntax"

	// filepathx supports double-star glob patterns (the stdlib doesn't). This
	// is necessary to match the behaviour from Bazel.
	"github.com/yargevad/filepathx"

	"github.com/bazelbuild/rules_python/gazelle/pythonconfig"
)

const languageName = "py"

const (
	// resolvedDepsKey is the attribute key used to pass dependencies that don't
	// need to be resolved by the dependency resolver in the Resolver step.
	resolvedDepsKey = "_gazelle_python_resolved_deps"
	// uuidKey is the attribute key used to uniquely identify a py_library
	// target that should be imported by a py_test or py_binary in the same
	// Bazel package.
	uuidKey = "_gazelle_python_library_uuid"
)

// Resolver satisfies the resolve.Resolver interface. It resolves dependencies
// in rules generated by this extension.
type Resolver struct{}

// Name returns the name of the language. This is the prefix of the kinds of
// rules generated. E.g. py_library and py_binary.
func (*Resolver) Name() string { return languageName }

// Imports returns a list of ImportSpecs that can be used to import the rule
// r. This is used to populate RuleIndex.
//
// If nil is returned, the rule will not be indexed. If any non-nil slice is
// returned, including an empty slice, the rule will be indexed.
func (py *Resolver) Imports(c *config.Config, r *rule.Rule, f *rule.File) []resolve.ImportSpec {
	cfgs := c.Exts[languageName].(pythonconfig.Configs)
	cfg := cfgs[f.Pkg]
	pythonProjectRoot := cfg.PythonProjectRoot()
	srcs, err := evalSrcsExpr(f.Pkg, r.Attr("srcs"))
	if err != nil {
		log.Fatalf("failed to process imports for %q in %q: %v", r.Name(), f.Pkg, err)
	}
	provides := make([]resolve.ImportSpec, 0, len(srcs)+1)
	for _, src := range srcs {
		ext := filepath.Ext(src)
		if ext == ".py" {
			provide := importSpecFromSrc(pythonProjectRoot, f.Pkg, src)
			provides = append(provides, provide)
		}
	}
	if r.PrivateAttr(uuidKey) != nil {
		provide := resolve.ImportSpec{
			Lang: languageName,
			Imp:  r.PrivateAttr(uuidKey).(string),
		}
		provides = append(provides, provide)
	}
	if len(provides) == 0 {
		return nil
	}
	return provides
}

// evalSrcsExpr returns the list of files in the srcs attribute. If the expr is
// a pure list expression, it's not evaluated as a starlark source. Otherwise,
// a starlark VM evaluates the expression, especially to resolve globs and other
// list arithmetic operations.
func evalSrcsExpr(pkg string, expr bzl.Expr) ([]string, error) {
	if list, ok := expr.(*bzl.ListExpr); ok {
		srcs := make([]string, 0, len(list.List))
		for _, e := range list.List {
			if str, ok := e.(*bzl.StringExpr); ok {
				srcs = append(srcs, str.Value)
			}
		}
		return srcs, nil
	}

	thread := &starlark.Thread{Load: repl.MakeLoad()}
	globber := Globber{pkg: pkg}
	env := starlark.StringDict{"glob": starlark.NewBuiltin("glob", globber.Glob)}
	srcsSyntaxExpr, err := syntax.ParseExpr("", bzl.FormatString(expr), syntax.RetainComments)
	if err != nil {
		return nil, fmt.Errorf("failed to eval srcs expression: %w", err)
	}
	srcsVal, err := starlark.EvalExpr(thread, srcsSyntaxExpr, env)
	if err != nil {
		return nil, fmt.Errorf("failed to eval srcs expression: %w", err)
	}
	srcsValList := srcsVal.(*starlark.List)
	srcs := make([]string, 0, srcsValList.Len())
	srcsValListIterator := srcsValList.Iterate()
	var srcVal starlark.Value
	for srcsValListIterator.Next(&srcVal) {
		src := srcVal.(starlark.String)
		srcs = append(srcs, string(src))
	}
	return srcs, nil
}

// Globber implements the glob built-in to evaluate the srcs attribute containing glob patterns.
type Globber struct {
	pkg string
}

// Glob expands the glob patterns and filters Bazel sub-packages from the tree.
// This is used to index manually created targets that contain globs so the
// resolution phase depends less on `gazelle:resolve` directives set by the
// user.
func (g *Globber) Glob(
	_ *starlark.Thread,
	_ *starlark.Builtin,
	args starlark.Tuple,
	kwargs []starlark.Tuple,
) (starlark.Value, error) {
	if len(args) > 1 {
		return nil, fmt.Errorf("failed glob: only 1 positional argument is allowed")
	}
	var includes starlark.Value
	if len(args) == 1 {
		includes = args[0]
	}
	var excludes starlark.Value
	for _, kwarg := range kwargs {
		switch kwarg[0] {
		case starlark.String("includes"):
			if includes != nil {
				return nil, fmt.Errorf("failed glob: invalid syntax: cannot use includes as kwarg and arg")
			}
			includes = kwarg[1]
		case starlark.String("excludes"):
			excludes = kwarg[1]
		default:
			return nil, fmt.Errorf("failed glob: invalid syntax: kwarg %q not recognized", kwarg[0])
		}
	}

	excludeSet := make(map[string]struct{})
	if excludes != nil {
		excludePatterns, ok := excludes.(*starlark.List)
		if !ok {
			return nil, fmt.Errorf("failed glob: excludes is not a list")
		}
		excludeIterator := excludePatterns.Iterate()
		var excludePatternVal starlark.Value
		for excludeIterator.Next(&excludePatternVal) {
			excludePattern, ok := excludePatternVal.(starlark.String)
			if !ok {
				return nil, fmt.Errorf("failed glob: exclude pattern must be a string")
			}
			matches, err := filepathx.Glob(path.Join(g.pkg, string(excludePattern)))
			if err != nil {
				return nil, fmt.Errorf("failed to get srcs: %w", err)
			}
			for _, match := range matches {
				exclude, _ := filepath.Rel(g.pkg, match)
				excludeSet[exclude] = struct{}{}
			}
		}
	}

	rootBazelPackageTree := NewBazelPackageTree(g.pkg)
	includePatterns, ok := includes.(*starlark.List)
	if !ok {
		return nil, fmt.Errorf("failed glob: includes is not a list")
	}
	includeIterator := includePatterns.Iterate()
	var includePatternVal starlark.Value
	for includeIterator.Next(&includePatternVal) {
		includePattern, ok := includePatternVal.(starlark.String)
		if !ok {
			return nil, fmt.Errorf("failed glob: include pattern must be a string")
		}
		matches, err := filepathx.Glob(path.Join(g.pkg, string(includePattern)))
		if err != nil {
			return nil, fmt.Errorf("failed to get srcs: %w", err)
		}
		for _, match := range matches {
			src, _ := filepath.Rel(g.pkg, match)
			if _, excluded := excludeSet[src]; !excluded {
				parts := strings.Split(src, string(filepath.Separator))
				rootBazelPackageTree.AddPath(parts)
			}
		}
	}
	return starlark.NewList(rootBazelPackageTree.Paths()), nil
}

// BazelPackageTree is a representation of a filesystem tree specialized for
// filtering paths that are under a Bazel sub-package. It understands the
// file-based boundaries that represent a sub-package (a nested BUILD file).
// The nature of this data structure also enables us to remove duplicated paths.
type BazelPackageTree struct {
	// pkg is the Bazel package this tree represents.
	pkg *string
	// branches is the connected branches of this tree, which is a recursive
	// field.
	branches map[string]*BazelPackageTree
	// isBazelPackage indicates whether this tree (which can also be considered
	// a "node" in the whole tree) is a Bazel package or not. This is used to
	// filter out sub-packages.
	isBazelPackage bool
	// isFile indicates whether this node is a leaf or not, so, when returning
	// the list of paths, we know append the part without joining it to the
	// child branches. This also enables constructing the paths without
	// returning partial paths during the recursion.
	isFile bool
}

// NewBazelPackageTree constructs a new BazelPackageTree.
func NewBazelPackageTree(pkg string) *BazelPackageTree {
	return &BazelPackageTree{
		pkg:      &pkg,
		branches: make(map[string]*BazelPackageTree),
	}
}

// AddPath adds a path to the package tree.
func (pt *BazelPackageTree) AddPath(parts []string) {
	branches := pt.branches
	for i, part := range parts {
		branch, exists := branches[part]
		if !exists {
			isFile := (i == len(parts)-1)
			var isBazelPkg bool
			if !isFile {
				dir := path.Join(parts[:i+1]...)
				dir = path.Join(*pt.pkg, dir)
				isBazelPkg = isBazelPackage(dir)
			}
			branch = &BazelPackageTree{
				pkg:            pt.pkg,
				branches:       make(map[string]*BazelPackageTree),
				isBazelPackage: isBazelPkg,
				isFile:         isFile,
			}
			branches[part] = branch
		}
		branches = branch.branches
	}
}

// Paths returns the list of paths in the tree, filtering Bazel sub-packages.
func (pt *BazelPackageTree) Paths() []starlark.Value {
	paths := make([]starlark.Value, 0)
	for part, branch := range pt.branches {
		if branch.isBazelPackage {
			continue
		}
		if branch.isFile {
			paths = append(paths, starlark.String(part))
		}
		for _, branchPath := range branch.Paths() {
			paths = append(paths, starlark.String(path.Join(part, string(branchPath.(starlark.String)))))
		}
	}
	return paths
}

// importSpecFromSrc determines the ImportSpec based on the target that contains the src so that
// the target can be indexed for import statements that match the calculated src relative to the its
// Python project root.
func importSpecFromSrc(pythonProjectRoot, bzlPkg, src string) resolve.ImportSpec {
	pythonPkgDir := filepath.Join(bzlPkg, filepath.Dir(src))
	relPythonPkgDir, err := filepath.Rel(pythonProjectRoot, pythonPkgDir)
	if err != nil {
		panic(fmt.Errorf("unexpected failure: %v", err))
	}
	if relPythonPkgDir == "." {
		relPythonPkgDir = ""
	}
	pythonPkg := strings.ReplaceAll(relPythonPkgDir, "/", ".")
	filename := filepath.Base(src)
	if filename == pyLibraryEntrypointFilename {
		if pythonPkg != "" {
			return resolve.ImportSpec{
				Lang: languageName,
				Imp:  pythonPkg,
			}
		}
	}
	moduleName := strings.TrimSuffix(filename, ".py")
	var imp string
	if pythonPkg == "" {
		imp = moduleName
	} else {
		imp = fmt.Sprintf("%s.%s", pythonPkg, moduleName)
	}
	return resolve.ImportSpec{
		Lang: languageName,
		Imp:  imp,
	}
}

// Embeds returns a list of labels of rules that the given rule embeds. If
// a rule is embedded by another importable rule of the same language, only
// the embedding rule will be indexed. The embedding rule will inherit
// the imports of the embedded rule.
func (py *Resolver) Embeds(r *rule.Rule, from label.Label) []label.Label {
	// TODO(f0rmiga): implement.
	return make([]label.Label, 0)
}

// Resolve translates imported libraries for a given rule into Bazel
// dependencies. Information about imported libraries is returned for each
// rule generated by language.GenerateRules in
// language.GenerateResult.Imports. Resolve generates a "deps" attribute (or
// the appropriate language-specific equivalent) for each import according to
// language-specific rules and heuristics.
func (py *Resolver) Resolve(
	c *config.Config,
	ix *resolve.RuleIndex,
	rc *repo.RemoteCache,
	r *rule.Rule,
	modulesRaw interface{},
	from label.Label,
) {
	// TODO(f0rmiga): may need to be defensive here once this Gazelle extension
	// join with the main Gazelle binary with other rules. It may conflict with
	// other generators that generate py_* targets.
	deps := treeset.NewWith(godsutils.StringComparator)
	if modulesRaw != nil {
		cfgs := c.Exts[languageName].(pythonconfig.Configs)
		cfg := cfgs[from.Pkg]
		pythonProjectRoot := cfg.PythonProjectRoot()
		modules := modulesRaw.(*treeset.Set)
		it := modules.Iterator()
		explainDependency := os.Getenv("EXPLAIN_DEPENDENCY")
		hasFatalError := false
	MODULE_LOOP:
		for it.Next() {
			mod := it.Value().(module)
			imp := resolve.ImportSpec{Lang: languageName, Imp: mod.Name}
			if override, ok := resolve.FindRuleWithOverride(c, imp, languageName); ok {
				if override.Repo == "" {
					override.Repo = from.Repo
				}
				if !override.Equal(from) {
					if override.Repo == from.Repo {
						override.Repo = ""
					}
					dep := override.String()
					deps.Add(dep)
					if explainDependency == dep {
						log.Printf("Explaining dependency (%s): "+
							"in the target %q, the file %q imports %q at line %d, "+
							"which resolves using the \"gazelle:resolve\" directive.\n",
							explainDependency, from.String(), mod.Filepath, mod.Name, mod.LineNumber)
					}
				}
			} else {
				if dep, ok := cfg.FindThirdPartyDependency(mod.Name); ok {
					deps.Add(dep)
					if explainDependency == dep {
						log.Printf("Explaining dependency (%s): "+
							"in the target %q, the file %q imports %q at line %d, "+
							"which resolves from the third-party module %q from the wheel %q.\n",
							explainDependency, from.String(), mod.Filepath, mod.Name, mod.LineNumber, mod.Name, dep)
					}
				} else {
					matches := ix.FindRulesByImportWithConfig(c, imp, languageName)
					if len(matches) == 0 {
						// Check if the imported module is part of the standard library.
						if isStd, err := isStdModule(mod); err != nil {
							log.Println("ERROR: ", err)
							hasFatalError = true
							continue MODULE_LOOP
						} else if isStd {
							continue MODULE_LOOP
						}
						if cfg.ValidateImportStatements() {
							err := fmt.Errorf(
								"%[1]q at line %[2]d from %[3]q is an invalid dependency: possible solutions:\n"+
									"\t1. Add it as a dependency in the requirements.txt file.\n"+
									"\t2. Instruct Gazelle to resolve to a known dependency using the gazelle:resolve directive.\n"+
									"\t3. Ignore it with a comment '# gazelle:ignore %[1]s' in the Python file.\n",
								mod.Name, mod.LineNumber, mod.Filepath,
							)
							log.Printf("ERROR: failed to validate dependencies for target %q: %v\n", from.String(), err)
							hasFatalError = true
							continue MODULE_LOOP
						}
					}
					filteredMatches := make([]resolve.FindResult, 0, len(matches))
					for _, match := range matches {
						if match.IsSelfImport(from) {
							// Prevent from adding itself as a dependency.
							continue MODULE_LOOP
						}
						filteredMatches = append(filteredMatches, match)
					}
					if len(filteredMatches) == 0 {
						continue
					}
					if len(filteredMatches) > 1 {
						sameRootMatches := make([]resolve.FindResult, 0, len(filteredMatches))
						for _, match := range filteredMatches {
							if strings.HasPrefix(match.Label.Pkg, pythonProjectRoot) {
								sameRootMatches = append(sameRootMatches, match)
							}
						}
						if len(sameRootMatches) != 1 {
							err := fmt.Errorf(
								"multiple targets (%s) may be imported with %q at line %d in %q "+
									"- this must be fixed using the \"gazelle:resolve\" directive",
								targetListFromResults(filteredMatches), mod.Name, mod.LineNumber, mod.Filepath)
							log.Println("ERROR: ", err)
							hasFatalError = true
							continue MODULE_LOOP
						}
						filteredMatches = sameRootMatches
					}
					matchLabel := filteredMatches[0].Label.Rel(from.Repo, from.Pkg)
					dep := matchLabel.String()
					deps.Add(dep)
					if explainDependency == dep {
						log.Printf("Explaining dependency (%s): "+
							"in the target %q, the file %q imports %q at line %d, "+
							"which resolves from the first-party indexed labels.\n",
							explainDependency, from.String(), mod.Filepath, mod.Name, mod.LineNumber)
					}
				}
			}
		}
		if hasFatalError {
			os.Exit(1)
		}
	}
	resolvedDeps := r.PrivateAttr(resolvedDepsKey).(*treeset.Set)
	if !resolvedDeps.Empty() {
		it := resolvedDeps.Iterator()
		for it.Next() {
			deps.Add(it.Value())
		}
	}
	if !deps.Empty() {
		r.SetAttr("deps", convertDependencySetToExpr(deps))
	}
}

// targetListFromResults returns a string with the human-readable list of
// targets contained in the given results.
func targetListFromResults(results []resolve.FindResult) string {
	list := make([]string, len(results))
	for i, result := range results {
		list[i] = result.Label.String()
	}
	return strings.Join(list, ", ")
}

// convertDependencySetToExpr converts the given set of dependencies to an
// expression to be used in the deps attribute.
func convertDependencySetToExpr(set *treeset.Set) bzl.Expr {
	deps := make([]bzl.Expr, set.Size())
	it := set.Iterator()
	for it.Next() {
		dep := it.Value().(string)
		deps[it.Index()] = &bzl.StringExpr{Value: dep}
	}
	return &bzl.ListExpr{List: deps}
}
