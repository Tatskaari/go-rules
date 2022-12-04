package generate

import (
	"fmt"
	"go/build"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/mod/modfile"

	bazelbuild "github.com/bazelbuild/buildtools/build"
	bazeledit "github.com/bazelbuild/buildtools/edit"
)

type Generate struct {
	moduleName         string
	srcRoot            string
	buildContext       build.Context
	buildFileName      string
	deps               []string
	pluginTarget       string
	replace            map[string]string
	knownImportTargets map[string]string // cache these so we don't end up looping over all the modules for every import
}

type rule struct {
	name          string
	kind          string
	srcs          []string
	cgoSrcs       []string
	compilerFlags []string
	linkerFlags   []string
	pkgConfigs    []string
	asmFiles      []string
	hdrs          []string
	deps          []string
	embedPatterns []string
}

func New(srcRoot string, requirements []string) *Generate {
	return &Generate{
		srcRoot:            srcRoot,
		buildContext:       build.Default,
		buildFileName:      "BUILD",
		deps:               requirements,
		knownImportTargets: map[string]string{},
	}
}

func (g *Generate) Generate() error {
	if err := g.readGoMod(); err != nil {
		return err
	}
	if err := g.writeConfig(); err != nil {
		return err
	}
	return g.generateAll(g.srcRoot)
}

// readGoMod reads the module dependencies
func (g *Generate) readGoMod() error {
	path := filepath.Join(g.srcRoot, "go.mod")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	modFile, err := modfile.ParseLax(path, data, nil)
	if err != nil {
		return err
	}

	// TODO we could probably validate these are known modules
	for _, req := range modFile.Require {
		g.deps = append(g.deps, req.Mod.Path)
	}

	g.moduleName = modFile.Module.Mod.Path
	g.deps = append(g.deps, g.moduleName)

	g.replace = make(map[string]string, len(modFile.Replace))
	for _, replace := range modFile.Replace {
		g.replace[replace.Old.Path] = replace.New.Path
	}
	return nil
}

func (g *Generate) writeConfig() error {
	file, err := os.Create(filepath.Join(g.srcRoot, ".plzconfig"))
	if err != nil {
		return err
	}
	defer file.Close()

	fmt.Fprintln(file, "[Plugin \"go\"]")
	fmt.Fprintln(file, "Target=@//plugins:go")
	fmt.Fprintf(file, "ImportPath=%s\n", g.moduleName)
	return nil
}

func (g *Generate) generateAll(dir string) error {
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == "testdata" {
				return filepath.SkipDir
			}
			if err := g.generate(filepath.Clean(strings.TrimPrefix(path, g.srcRoot))); err != nil {
				switch err.(type) {
				case *build.NoGoError:
					// We might walk into a dir that has no .go files for the current arch. This shouldn't
					// be an error so we just eat this
					return nil
				default:
					return err
				}
			}
		}
		return nil
	})
}

func (g *Generate) pkgDir(target string) string {
	p := strings.TrimPrefix(target, g.moduleName)
	return filepath.Join(g.srcRoot, p)
}

func (g *Generate) importDir(target string) (*build.Package, error) {
	dir := filepath.Join(os.Getenv("TMP_DIR"), g.pkgDir(target))
	return g.buildContext.ImportDir(dir, build.ImportComment)
}

func (g *Generate) generate(dir string) error {
	pkg, err := g.importDir(dir)
	if err != nil {
		return err
	}
	return g.writeFile(dir, g.newRule(pkg))
}

func (g *Generate) writeFile(pkg string, rules []*rule) error {
	path := filepath.Join(g.pkgDir(pkg), g.buildFileName)
	f, err := os.Create(filepath.Join(g.pkgDir(pkg), g.buildFileName))
	if err != nil {
		return err
	}
	defer f.Close()

	buildFile, err := bazelbuild.Parse(path, nil)
	if err != nil {
		return err
	}

	buildFile.Stmt = []bazelbuild.Expr{
		&bazelbuild.CallExpr{
			X: &bazelbuild.Ident{Name: "subinclude"},
			List: []bazelbuild.Expr{
				NewStringExpr("///go//build_defs:go"),
			},
		},
	}

	for _, rule := range rules {
		r := NewRule(buildFile, rule.kind, rule.name)
		if len(rule.cgoSrcs) > 0 {
			r.SetAttr("srcs", NewStringList(rule.cgoSrcs))
			r.SetAttr("go_srcs", NewStringList(rule.srcs))
		} else {
			r.SetAttr("srcs", NewStringList(rule.srcs))
		}
		if len(rule.deps) > 0 {
			r.SetAttr("deps", NewStringList(rule.deps))
		}
		if len(rule.compilerFlags) > 0 {
			r.SetAttr("pkg_config", NewStringList(rule.pkgConfigs))
		}
		if len(rule.compilerFlags) > 0 {
			r.SetAttr("compiler_flags", NewStringList(rule.compilerFlags))
		}
		if len(rule.linkerFlags) > 0 {
			r.SetAttr("linker_flags", NewStringList(rule.linkerFlags))
		}
		if len(rule.hdrs) > 0 {
			r.SetAttr("hdrs", NewStringList(rule.hdrs))
		}
		if len(rule.asmFiles) > 0 {
			r.SetAttr("asm_srcs", NewStringList(rule.asmFiles))
		}
		if len(rule.deps) > 0 {
			r.SetAttr("deps", NewStringList(rule.deps))
		}
		if len(rule.embedPatterns) > 0 {
			r.SetAttr("resources", &bazelbuild.CallExpr{
				X: &bazelbuild.Ident{Name: "glob"},
				List: []bazelbuild.Expr{
					NewStringList(rule.embedPatterns),
				},
			})
		}
		r.SetAttr("visibility", NewStringList([]string{"PUBLIC"}))
	}

	_, err = f.Write(bazelbuild.Format(buildFile))
	return err
}

func NewRule(f *bazelbuild.File, kind, name string) *bazelbuild.Rule {
	rule, _ := bazeledit.ExprToRule(&bazelbuild.CallExpr{
		X:    &bazelbuild.Ident{Name: kind},
		List: []bazelbuild.Expr{},
	}, kind)

	rule.SetAttr("name", NewStringExpr(name))

	f.Stmt = append(f.Stmt, rule.Call)
	return rule
}

func NewStringExpr(s string) *bazelbuild.StringExpr {
	return &bazelbuild.StringExpr{Value: s}
}

func NewStringList(ss []string) *bazelbuild.ListExpr {
	l := new(bazelbuild.ListExpr)
	for _, s := range ss {
		l.List = append(l.List, NewStringExpr(s))
	}
	return l
}

func packageKind(pkg *build.Package) string {
	cgo := len(pkg.CgoFiles) > 0
	if pkg.IsCommand() && cgo {
		return "cgo_binary"
	}
	if pkg.IsCommand() {
		return "go_binary"
	}
	if cgo {
		return "cgo_library"
	}
	return "go_library"
}

func (g *Generate) depTargets(imports []string) []string {
	deps := make([]string, 0)
	for _, path := range imports {
		target := g.depTarget(path)
		if target == "" {
			continue
		}
		deps = append(deps, target)
	}
	return deps
}

func (g *Generate) newRule(pkg *build.Package) []*rule {

	// The name of the target should match the dir it's in, or the basename of the module if it's in the repo root.
	name := filepath.Base(pkg.Dir)
	if strings.HasSuffix(pkg.Dir, g.srcRoot) || name == "" {
		name = filepath.Base(g.moduleName)
	}

	if name == "." {
		panic(fmt.Sprintf("%v %v", g.moduleName, pkg.Dir))
	}
	var rules []*rule

	if len(pkg.GoFiles) > 0 || len(pkg.CgoFiles) > 0 {
		packageRule := &rule{
			name:          name,
			kind:          packageKind(pkg),
			srcs:          pkg.GoFiles,
			cgoSrcs:       pkg.CgoFiles,
			compilerFlags: pkg.CgoCFLAGS,
			linkerFlags:   pkg.CgoLDFLAGS,
			pkgConfigs:    pkg.CgoPkgConfig,
			asmFiles:      pkg.SFiles,
			hdrs:          pkg.HFiles,
			deps:          g.depTargets(pkg.Imports),
			embedPatterns: pkg.EmbedPatterns,
		}
		rules = append(rules, packageRule)
	}
	return rules
}

func (g *Generate) depTarget(importPath string) string {
	if target, ok := g.knownImportTargets[importPath]; ok {
		return target
	}

	if replacement, ok := g.replace[importPath]; ok {
		target := g.depTarget(replacement)
		g.knownImportTargets[importPath] = target
		return target
	}

	module := ""

	// TODO a trie would be a more sensible data structure here
	for _, mod := range g.deps {
		if strings.HasPrefix(importPath, mod) {
			if len(module) < len(mod) {
				module = mod
			}
		}
	}

	if module == "" {
		// If we can't find this import, we can return nothing and the build rule will fail at build time reporting a
		// sensible error. It may also be an import from the go SDK which is fine.
		return ""
	}

	subrepoName := strings.ReplaceAll(module, "/", "_")

	packageName := strings.TrimPrefix(importPath, module)
	packageName = strings.TrimPrefix(packageName, "/")

	target := ""
	if packageName == "" {
		target = fmt.Sprintf("///third_party/go/%s//:%s", subrepoName, filepath.Base(module))
	} else {
		target = fmt.Sprintf("///third_party/go/%s//%s:%s", subrepoName, packageName, filepath.Base(packageName))
	}

	g.knownImportTargets[importPath] = target
	return target
}
