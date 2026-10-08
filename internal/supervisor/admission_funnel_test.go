package supervisor

// The Admission Funnel, Read From The Source
//
// SPEC-0021 REQ-4: every process start passes admission first. A behaviour
// test can only cover the start paths it knows about, and the bug the design
// warns of is the ninth path nobody wrote a test for ("a check added to eight
// of them is the bug"). So this test reads the package's source instead, and
// holds every path to the funnel structurally:
//
//  1. Only spawn and startOnPipes exec a process (exec.Command and friends).
//  2. Only beginStart calls spawn, and only spawn calls startOnPipes.
//  3. An admission (the value spawn requires) is created only in admitStart,
//     which asks the Manager's funnel; nowhere else builds, news or declares
//     one.
//  4. Every call that hands an admission on passes either a value assigned
//     from an admission source (admitStart, or a function that returns what
//     admitStart returns) or the caller's own admission parameter, whose
//     callers are held to the same rule.
//
// A new start path that skips admission breaks one of the four, whichever way
// it is written. It lists every spawn and exec site, and every hand-off of an
// admission, as it goes.
//
// Governing: ADR-0027; SPEC-0021 REQ-4 "Admission", REQ-21; design.md §
// "Admission is a funnel on the Manager".
//
// @joestump 10/04/2026 - Added for stump.wtf/harness#470.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// funnelSource is the package's non-test source, parsed.
type funnelSource struct {
	fset  *token.FileSet
	files []*ast.File
}

func parseFunnelSource(t *testing.T, dir string) funnelSource {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := funnelSource{fset: token.NewFileSet()}
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(src.fset, p, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		src.files = append(src.files, f)
	}
	if len(src.files) == 0 {
		t.Fatalf("no source in %s", dir)
	}
	return src
}

// eachFunc calls fn for every function and method declaration with a body.
func (src funnelSource) eachFunc(fn func(*ast.FuncDecl)) {
	for _, f := range src.files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
				fn(fd)
			}
		}
	}
}

func (src funnelSource) pos(n ast.Node) string {
	p := src.fset.Position(n.Pos())
	return filepath.Base(p.Filename) + ":" + itoa(p.Line)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

// calleeName is a call's function name: "spawn" for spawn(...),
// "beginStart" for s.beginStart(...), "exec.Command" for a package call.
func calleeName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		if pkg, ok := fn.X.(*ast.Ident); ok && (pkg.Name == "exec" || pkg.Name == "os" || pkg.Name == "syscall" || pkg.Name == "pty") {
			return pkg.Name + "." + fn.Sel.Name
		}
		return fn.Sel.Name
	}
	return ""
}

// isAdmissionType reports a type expression naming admission or *admission.
func isAdmissionType(e ast.Expr) bool {
	if st, ok := e.(*ast.StarExpr); ok {
		e = st.X
	}
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "admission"
}

// funnelReport is what the walk found and what it objects to.
type funnelReport struct {
	listing    []string
	violations []string
}

func (r *funnelReport) list(format string, args ...string) {
	r.listing = append(r.listing, fmtArgs(format, args...))
}

func (r *funnelReport) fail(format string, args ...string) {
	r.violations = append(r.violations, fmtArgs(format, args...))
}

func fmtArgs(format string, args ...string) string {
	for _, a := range args {
		format = strings.Replace(format, "%s", a, 1)
	}
	return format
}

// checkFunnel applies the four rules to src.
func checkFunnel(src funnelSource) funnelReport {
	var r funnelReport
	// The processes this package may start, and the only functions allowed
	// to start them.
	execCalls := map[string]bool{
		"exec.Command": true, "exec.CommandContext": true, "os.StartProcess": true,
		"syscall.ForkExec": true, "syscall.StartProcess": true, "syscall.Exec": true,
		"pty.Start": true,
	}
	execOwners := map[string]bool{"spawn": true, "startOnPipes": true}
	spawnCallers := map[string][]string{"spawn": {"beginStart"}, "startOnPipes": {"spawn"}}
	const mint = "admitStart"

	// Pass 1: which functions take an admission (and at which argument), and
	// which hand one back (admission sources).
	takes := map[string]int{}
	returns := map[string]*ast.FuncDecl{}
	src.eachFunc(func(fd *ast.FuncDecl) {
		i := 0
		for _, field := range fd.Type.Params.List {
			n := max(len(field.Names), 1)
			if isAdmissionType(field.Type) {
				takes[fd.Name.Name] = i
			}
			i += n
		}
		if fd.Type.Results != nil {
			for _, field := range fd.Type.Results.List {
				if isAdmissionType(field.Type) {
					returns[fd.Name.Name] = fd
				}
			}
		}
	})
	if _, ok := takes["spawn"]; !ok {
		r.fail("spawn no longer requires an admission: every start must carry one to exec")
	}
	if _, ok := returns[mint]; !ok {
		r.fail("%s no longer returns an admission", mint)
	}

	// An admission source is admitStart, or a function every return of which
	// hands back an admission source's result (or nil).
	sources := map[string]bool{mint: true}
	for changed := true; changed; {
		changed = false
		for name, fd := range returns {
			if sources[name] {
				continue
			}
			ok := true
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				ret, isRet := n.(*ast.ReturnStmt)
				if !isRet || len(ret.Results) == 0 {
					return !isRet
				}
				switch v := ret.Results[0].(type) {
				case *ast.CallExpr:
					ok = ok && sources[calleeName(v)]
				case *ast.Ident:
					ok = ok && v.Name == "nil"
				default:
					ok = false
				}
				return false
			})
			if ok {
				sources[name] = true
				changed = true
			}
		}
	}
	for name := range returns {
		if !sources[name] {
			r.fail("%s returns an admission that does not come from %s", name, mint)
		}
	}

	src.eachFunc(func(fd *ast.FuncDecl) {
		fn := fd.Name.Name
		// The admissions fn may hand on: its own parameter, and every
		// variable it assigned from an admission source.
		admitted := map[string]bool{}
		for _, field := range fd.Type.Params.List {
			if isAdmissionType(field.Type) {
				for _, n := range field.Names {
					admitted[n.Name] = true
				}
			}
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok || len(as.Rhs) != 1 {
				return true
			}
			if call, ok := as.Rhs[0].(*ast.CallExpr); ok && sources[calleeName(call)] {
				if id, ok := as.Lhs[0].(*ast.Ident); ok {
					admitted[id.Name] = true
				}
			}
			return true
		})

		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CompositeLit:
				// Rule 3: only the mint builds an admission.
				if isAdmissionType(x.Type) && fn != mint {
					r.fail("%s: %s builds an admission; only %s may (it asks the funnel)", src.pos(x), fn, mint)
				}
			case *ast.ValueSpec:
				if x.Type != nil && isAdmissionType(x.Type) {
					r.fail("%s: %s declares an admission; take one from %s", src.pos(x), fn, mint)
				}
			case *ast.CallExpr:
				name := calleeName(x)
				if name == "new" && len(x.Args) == 1 && isAdmissionType(x.Args[0]) {
					r.fail("%s: %s news an admission; only %s may build one", src.pos(x), fn, mint)
				}
				// Rule 1: exec sites.
				if execCalls[name] {
					r.list("exec   %s in %s at %s", name, fn, src.pos(x))
					if !execOwners[fn] {
						r.fail("%s: %s starts a process (%s) outside spawn: it skips admission", src.pos(x), fn, name)
					}
				}
				// Rule 2: spawn's callers.
				if allowed, ok := spawnCallers[name]; ok {
					r.list("spawn  %s from %s at %s", name, fn, src.pos(x))
					if !slices.Contains(allowed, fn) {
						r.fail("%s: %s calls %s; only %s may", src.pos(x), fn, name, strings.Join(allowed, ", "))
					}
				}
				// Rule 4: every hand-off of an admission.
				if i, ok := takes[name]; ok {
					if i >= len(x.Args) {
						r.fail("%s: %s calls %s without its admission", src.pos(x), fn, name)
						break
					}
					arg := x.Args[i]
					id, isIdent := arg.(*ast.Ident)
					switch {
					case isIdent && admitted[id.Name]:
						r.list("admit  %s(%s) from %s at %s", name, id.Name, fn, src.pos(x))
					default:
						r.fail("%s: %s hands %s an admission that did not come from %s", src.pos(x), fn, name, mint)
					}
				}
			}
			return true
		})
	})
	return r
}

// TestEveryStartPathIsAdmitted holds the package's own source to the funnel,
// and lists what it checked.
func TestEveryStartPathIsAdmitted(t *testing.T) {
	r := checkFunnel(parseFunnelSource(t, "."))
	for _, l := range r.listing {
		t.Log(l)
	}
	for _, v := range r.violations {
		t.Error(v)
	}
	// The check must have something to check: a refactor that renamed spawn
	// or the mint would otherwise pass vacuously (CLAUDE.md "A zero").
	var spawns, execs, handoffs int
	for _, l := range r.listing {
		switch {
		case strings.HasPrefix(l, "spawn "):
			spawns++
		case strings.HasPrefix(l, "exec "):
			execs++
		case strings.HasPrefix(l, "admit "):
			handoffs++
		}
	}
	if spawns < 2 || execs < 2 || handoffs < 4 {
		t.Fatalf("checked %d spawn calls, %d exec sites and %d admission hand-offs; the walk lost its targets", spawns, execs, handoffs)
	}
}

// TestFunnelCheckCatchesABypass proves the check can fire: a copy of the
// package's source with one new start path added each way it could skip
// admission must fail, naming the path.
func TestFunnelCheckCatchesABypass(t *testing.T) {
	for _, c := range []struct {
		name, code, want string
	}{
		{"a literal admission", `func (s *Supervisor) bypass() { s.beginStart(&admission{}) }`, "bypass builds an admission"},
		{"a nil admission", `func (s *Supervisor) bypass() { var adm *admission; s.beginStart(adm) }`, "bypass declares an admission"},
		{"a direct spawn", `func (s *Supervisor) bypass() { _, _ = spawn(nil, s.harness, 80, 24, RunEnv{}) }`, "bypass calls spawn"},
		{"a raw exec", `func (s *Supervisor) bypass() { _ = exec.Command("sh") }`, "bypass starts a process (exec.Command)"},
		{"a forged source", `func (s *Supervisor) forge() *admission { return new(admission) }
func (s *Supervisor) bypass() { adm := s.forge(); s.beginStart(adm) }`, "forge returns an admission that does not come from admitStart"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			entries, err := os.ReadDir(".")
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
					continue
				}
				b, err := os.ReadFile(e.Name())
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, e.Name()), b, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			bypass := "package supervisor\n\nimport \"os/exec\"\n\nvar _ = exec.Command\n\n" + c.code + "\n"
			if err := os.WriteFile(filepath.Join(dir, "bypass.go"), []byte(bypass), 0o600); err != nil {
				t.Fatal(err)
			}
			r := checkFunnel(parseFunnelSource(t, dir))
			if !slices.ContainsFunc(r.violations, func(v string) bool { return strings.Contains(v, c.want) }) {
				t.Fatalf("the check passed a bypass (%s); violations: %q", c.name, r.violations)
			}
		})
	}
}
