package grizzle_test

import (
	"encoding/json"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

type pkgInfo struct {
	ImportPath string   `json:"ImportPath"`
	Imports    []string `json:"Imports"`
	Deps       []string `json:"Deps"`
}

func TestArchitecture_DependencyRules(t *testing.T) {
	cmd := exec.Command("go", "list", "-json", "./...")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list failed: %v", err)
	}

	dec := json.NewDecoder(strings.NewReader(string(out)))
	pkgs := make(map[string]pkgInfo)

	for dec.More() {
		var p pkgInfo
		if err := dec.Decode(&p); err != nil {
			t.Fatalf("decode package info: %v", err)
		}
		pkgs[p.ImportPath] = p
	}

	const (
		modulePrefix = "github.com/muandane/grizzle"
		schemaPkg    = modulePrefix + "/internal/schema"
		scopePkg     = modulePrefix + "/internal/scope"
		planPkg      = modulePrefix + "/internal/plan"
		diffPkg      = modulePrefix + "/internal/diff"
		dialectPkg   = modulePrefix + "/internal/dialect"
		execPkg      = modulePrefix + "/internal/exec"
		rootPkg      = modulePrefix
	)

	// 1. Pure layer rules: schema, scope, plan, diff must NEVER import database/sql or context
	purePackages := []string{schemaPkg, scopePkg, planPkg, diffPkg}
	for _, pPath := range purePackages {
		p, exists := pkgs[pPath]
		if !exists {
			t.Fatalf("package %q not found in go list", pPath)
		}
		if slices.Contains(p.Imports, "database/sql") {
			t.Errorf("pure package %q illegally imports database/sql", pPath)
		}
		if slices.Contains(p.Imports, "context") {
			t.Errorf("pure package %q illegally imports context", pPath)
		}
	}

	// 2. schema layer: must not import any other internal packages
	for _, imp := range pkgs[schemaPkg].Imports {
		if strings.HasPrefix(imp, modulePrefix+"/internal/") {
			t.Errorf("internal/schema illegally imports internal package: %s", imp)
		}
	}

	// 3. scope layer: can only import schema from internal
	for _, imp := range pkgs[scopePkg].Imports {
		if strings.HasPrefix(imp, modulePrefix+"/internal/") && imp != schemaPkg {
			t.Errorf("internal/scope illegally imports: %s", imp)
		}
	}

	// 4. plan layer: can only import schema from internal
	for _, imp := range pkgs[planPkg].Imports {
		if strings.HasPrefix(imp, modulePrefix+"/internal/") && imp != schemaPkg {
			t.Errorf("internal/plan illegally imports: %s", imp)
		}
	}

	// 5. diff layer: can only import schema, scope, plan from internal
	allowedForDiff := []string{schemaPkg, scopePkg, planPkg}
	for _, imp := range pkgs[diffPkg].Imports {
		if strings.HasPrefix(imp, modulePrefix+"/internal/") && !slices.Contains(allowedForDiff, imp) {
			t.Errorf("internal/diff illegally imports: %s", imp)
		}
	}

	// 6. dialect layer: must NOT import exec, history, or root package
	for path, p := range pkgs {
		if strings.HasPrefix(path, dialectPkg) {
			for _, imp := range p.Imports {
				if strings.HasPrefix(imp, execPkg) {
					t.Errorf("dialect package %q illegally imports exec package %q", path, imp)
				}
				if imp == rootPkg {
					t.Errorf("dialect package %q illegally imports root package %q", path, imp)
				}
			}
		}
	}

	// 7. exec layer: must NOT import root package
	for _, imp := range pkgs[execPkg].Imports {
		if imp == rootPkg {
			t.Errorf("exec package %q illegally imports root package %q", execPkg, imp)
		}
	}
}
