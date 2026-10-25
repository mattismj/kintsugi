// The blueprint tests are an EXTERNAL test package on purpose.
//
// internal/lab imports internal/blueprints (its dormant "the blueprint ships a
// compose file" path renders the templates to find one), so an internal
// `package blueprints` test file may not import internal/lab back: Go rejects
// the cycle. These tests need the real lab harness — FromConfig, ComposePath,
// Up, Down — to prove a rendered workspace is one `kintsugi lab up` accepts, so
// they live in blueprints_test and qualify every blueprint identifier.
package blueprints_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"gopkg.in/yaml.v3"

	"kintsugi/internal/blueprints"
	"kintsugi/internal/config"
	fctx "kintsugi/internal/context"
	"kintsugi/internal/lab"
)

// requiredHeadings are the CONTEXT.md sections every blueprint must ship, in
// order: Authorization & scope and Domain context come first because they are
// the two a model must read before anything else (DESIGN §4, §6.2).
var requiredHeadings = []string{
	"## Authorization & scope",
	"## Domain context",
	"## Stack",
	"## Hard conventions",
	"## Current state",
	"## Lessons (active)",
}

// scaffolded are the files every blueprint writes, in template order
// (WalkDir visits lexically, so Render's lists are sorted the same way).
//
// The lab compose file is part of the scaffold, not an optional extra: a
// workspace `kintsugi new` produces must be one `kintsugi lab up` works in today.
var scaffolded = []string{
	blueprints.ContextFile,
	blueprints.ReadmeFile,
	blueprints.FixturesFile,
	blueprints.ConfigFile,
	lab.DefaultComposeFile,
}

// inventedClaims are authorization assertions the harness must never make up on
// the operator's behalf.
var inventedClaims = []string{
	"authorized",
	"we have permission",
	"is permitted",
	"licensed",
	"penetration test",
	"red-team",
	"dual-use",
	"rules of engagement",
	"approved by",
}

// composeDoc is the shape these tests check the lab compose file against. Only
// the fields that carry a safety claim are decoded; an unknown key is ignored
// here and caught by the "no host escape hatches" assertions below.
type composeDoc struct {
	Services map[string]composeService `yaml:"services"`
	Networks map[string]composeNetwork `yaml:"networks"`
	Volumes  map[string]any            `yaml:"volumes"`
}

type composeService struct {
	Image       string         `yaml:"image"`
	NetworkMode string         `yaml:"network_mode"`
	Networks    map[string]any `yaml:"networks"`
	Ports       []any          `yaml:"ports"`
	Expose      []any          `yaml:"expose"`
	CapAdd      []any          `yaml:"cap_add"`
	CapDrop     []string       `yaml:"cap_drop"`
	Privileged  bool           `yaml:"privileged"`
	ReadOnly    bool           `yaml:"read_only"`
	Healthcheck struct {
		Test any `yaml:"test"`
	} `yaml:"healthcheck"`
}

type composeNetwork struct {
	Internal bool `yaml:"internal"`
}

func TestList(t *testing.T) {
	got := blueprints.List()
	if len(got) != 4 {
		t.Fatalf("List() returned %d blueprints, want 4: %+v", len(got), got)
	}
	if got[0].Name != "c2" || got[1].Name != "recon" || got[2].Name != "detection" || got[3].Name != "redteam" {
		t.Fatalf("List() = [%s %s %s %s], want [c2 recon detection redteam]", got[0].Name, got[1].Name, got[2].Name, got[3].Name)
	}
	for _, b := range got {
		if strings.TrimSpace(b.Summary) == "" {
			t.Errorf("blueprint %q has no summary", b.Name)
		}
	}
	if strings.Join(blueprints.Names(), ",") != "c2,recon,detection,redteam" {
		t.Errorf("Names() = %v", blueprints.Names())
	}

	// List is a copy: mutating it must not corrupt the catalog.
	got[0].Name = "mutated"
	if again := blueprints.List(); again[0].Name != "c2" {
		t.Errorf("List() shares its backing array with the catalog: %q", again[0].Name)
	}

	for _, name := range blueprints.Names() {
		if _, ok := blueprints.Lookup(name); !ok {
			t.Errorf("Lookup(%q) not found", name)
		}
	}
	if _, ok := blueprints.Lookup("listener"); ok {
		t.Error("Lookup(\"listener\") found a blueprint beyond the cut line")
	}
}

func TestRenderScaffoldsEveryFile(t *testing.T) {
	for _, name := range blueprints.Names() {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			written, skipped, err := blueprints.Render(name, dir, "demo", false)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if strings.Join(written, ",") != strings.Join(scaffolded, ",") {
				t.Errorf("written = %v, want %v", written, scaffolded)
			}
			if len(skipped) != 0 {
				t.Errorf("skipped = %v, want none", skipped)
			}
			for _, rel := range scaffolded {
				st, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel)))
				if err != nil {
					t.Fatalf("%s not created: %v", rel, err)
				}
				if st.IsDir() {
					t.Fatalf("%s is a directory", rel)
				}
				if st.Size() == 0 {
					t.Errorf("%s is empty", rel)
				}
			}
		})
	}
}

func TestContextHasEverySection(t *testing.T) {
	for _, name := range blueprints.Names() {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if _, _, err := blueprints.Render(name, dir, "demo", false); err != nil {
				t.Fatalf("Render: %v", err)
			}
			body := readFile(t, filepath.Join(dir, blueprints.ContextFile))

			if !strings.HasPrefix(body, "# Project: demo\n") {
				t.Errorf("missing `# Project: <name>` preamble:\n%s", body)
			}
			for _, h := range requiredHeadings {
				if !strings.Contains(body, "\n"+h+"\n") {
					t.Errorf("missing heading %q in:\n%s", h, body)
				}
			}
			// Order check: the two load-bearing sections come first.
			auth := strings.Index(body, "## Authorization & scope")
			dom := strings.Index(body, "## Domain context")
			stack := strings.Index(body, "## Stack")
			if !(auth < dom && dom < stack) {
				t.Errorf("section order wrong (auth=%d domain=%d stack=%d)", auth, dom, stack)
			}

			// The document must parse into the classes the compiler expects.
			doc, warns := fctx.Parse([]byte(body))
			if len(warns) != 0 {
				t.Errorf("CONTEXT.md parses with warnings: %v", warns)
			}
			if doc.Project != "demo" {
				t.Errorf("parsed project = %q, want demo", doc.Project)
			}
			for _, class := range []string{
				fctx.ClassAuthorization, fctx.ClassDomain, fctx.ClassStack,
				fctx.ClassConvention, fctx.ClassState, fctx.ClassLesson,
			} {
				if _, ok := doc.Find(class); !ok {
					t.Errorf("section class %q not parseable from CONTEXT.md", class)
				}
			}

			// Current state is the one section with fixed wording.
			if s, ok := doc.Find(fctx.ClassState); ok {
				if got := strings.TrimSpace(s.Body); got != "- Fresh blueprint; nothing recorded yet." {
					t.Errorf("Current state = %q, want the fixed fresh-blueprint line", got)
				}
			}
		})
	}
}

func TestAuthorizationIsOperatorPlaceholderOnly(t *testing.T) {
	for _, name := range blueprints.Names() {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if _, _, err := blueprints.Render(name, dir, "demo", false); err != nil {
				t.Fatalf("Render: %v", err)
			}
			doc, _ := fctx.Parse([]byte(readFile(t, filepath.Join(dir, blueprints.ContextFile))))
			sec, ok := doc.Find(fctx.ClassAuthorization)
			if !ok {
				t.Fatal("no Authorization & scope section")
			}
			if !strings.Contains(sec.Body, blueprints.OperatorPlaceholder) {
				t.Fatalf("authorization body lacks the operator placeholder:\n%s", sec.Body)
			}
			if !strings.Contains(sec.Body, "- TODO:") {
				t.Fatalf("authorization body lacks a TODO bullet:\n%s", sec.Body)
			}
			// Exactly one TODO bullet: a placeholder, not a drafted policy.
			if n := strings.Count(sec.Body, "\n- "); n != 1 {
				t.Errorf("authorization body has %d bullets, want 1:\n%s", n, sec.Body)
			}
			low := strings.ToLower(sec.Body)
			for _, claim := range inventedClaims {
				if strings.Contains(low, claim) {
					t.Errorf("authorization body invents a claim (%q):\n%s", claim, sec.Body)
				}
			}
			if n := len(strings.Fields(sec.Body)); n > 60 {
				t.Errorf("authorization body is %d words — too long for a placeholder:\n%s", n, sec.Body)
			}
			if !fctx.HardGated(sec.Class) {
				t.Error("Authorization & scope is not hard-gated")
			}
		})
	}
}

func TestProjectTokenSubstituted(t *testing.T) {
	for _, name := range blueprints.Names() {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if _, _, err := blueprints.Render(name, dir, "lab-tasking", false); err != nil {
				t.Fatalf("Render: %v", err)
			}
			for _, rel := range scaffolded {
				body := readFile(t, filepath.Join(dir, filepath.FromSlash(rel)))
				if strings.Contains(body, blueprints.ProjectToken) {
					t.Errorf("%s still contains the %s token", rel, blueprints.ProjectToken)
				}
			}
			if !strings.Contains(readFile(t, filepath.Join(dir, blueprints.ContextFile)), "# Project: lab-tasking") {
				t.Error("project name not substituted into CONTEXT.md")
			}
			if !strings.Contains(readFile(t, filepath.Join(dir, blueprints.ReadmeFile)), "lab-tasking") {
				t.Error("project name not substituted into README.md")
			}
			if !strings.Contains(readFile(t, filepath.Join(dir, blueprints.FixturesFile)), "lab-tasking") {
				t.Error("project name not substituted into fixtures/README.md")
			}
			// The compose file names the project too, so a lab that is up can
			// be traced back to the workspace it came from.
			if !strings.Contains(readFile(t, filepath.Join(dir, filepath.FromSlash(lab.DefaultComposeFile))), "lab-tasking") {
				t.Error("project name not substituted into " + lab.DefaultComposeFile)
			}
		})
	}
}

func TestConfigProfile(t *testing.T) {
	for _, name := range blueprints.Names() {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if _, _, err := blueprints.Render(name, dir, "demo", false); err != nil {
				t.Fatalf("Render: %v", err)
			}
			cfg := readFile(t, filepath.Join(dir, blueprints.ConfigFile))
			for _, want := range []string{
				"[workspace]",
				`profile = "lab"`,
				"[core]",
				`mode = "offline"`,
			} {
				if !strings.Contains(cfg, want) {
					t.Errorf("kintsugi.toml missing %q:\n%s", want, cfg)
				}
			}
			// default_model must be a commented line, not a binding.
			var commented bool
			for _, line := range strings.Split(cfg, "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "# default_model = ") {
					commented = true
				}
				if strings.HasPrefix(line, "default_model") {
					t.Errorf("kintsugi.toml binds default_model instead of commenting it: %q", line)
				}
			}
			if !commented {
				t.Errorf("kintsugi.toml has no commented default_model line:\n%s", cfg)
			}
		})
	}
}

func TestConfigDecodes(t *testing.T) {
	for _, name := range blueprints.Names() {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if _, _, err := blueprints.Render(name, dir, "demo", false); err != nil {
				t.Fatalf("Render: %v", err)
			}
			// Decode against the real config struct so an unknown key is a test
			// failure rather than a silently ignored line.
			cfg := config.Defaults()
			md, err := toml.DecodeFile(filepath.Join(dir, blueprints.ConfigFile), &cfg)
			if err != nil {
				t.Fatalf("kintsugi.toml does not parse: %v", err)
			}
			if keys := md.Undecoded(); len(keys) > 0 {
				t.Errorf("kintsugi.toml has keys config does not know: %v", keys)
			}
			if cfg.Workspace.Profile != "lab" {
				t.Errorf("workspace.profile = %q, want lab", cfg.Workspace.Profile)
			}
			if cfg.Core.Mode != "offline" {
				t.Errorf("core.mode = %q, want offline", cfg.Core.Mode)
			}
		})
	}
}

// TestRenderShipsTheLabComposeFile is the M8-A contract: every blueprint ships
// a compose file at the path the lab harness defaults to, so `kintsugi new <bp>`
// followed by `kintsugi lab up` works without the operator writing anything.
//
// The file has to be honest about what it is. A lab that claims to be isolated
// and is not would be worse than no lab, so the safety claims are asserted
// against the parsed YAML rather than against a comment: one service named
// `fixture`, no published ports, no network at all, a healthcheck, and none of
// the escape hatches that would put the host back in reach.
func TestRenderShipsTheLabComposeFile(t *testing.T) {
	for _, name := range blueprints.Names() {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			written, _, err := blueprints.Render(name, dir, "demo", false)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if !contains(written, lab.DefaultComposeFile) {
				t.Fatalf("written = %v, want it to include %s", written, lab.DefaultComposeFile)
			}

			path := filepath.Join(dir, filepath.FromSlash(lab.DefaultComposeFile))
			body := readFile(t, path)
			doc := parseCompose(t, body)

			// The c2/recon shape is one inert fixture service; detection is a
			// generator + receiver pair; redteam is two vulnerable targets.
			// Each lab is as small as its purpose allows — no service beyond
			// the blueprint's own shape.
			want := map[string]int{"c2": 1, "recon": 1, "detection": 2, "redteam": 2}
			if len(doc.Services) != want[name] {
				t.Fatalf("%s: the compose file declares %d services (%v), want %d",
					name, len(doc.Services), keys(doc.Services), want[name])
			}
			// Every declared service obeys the safety rules — not just a named
			// "fixture" one: detection ships a generator + receiver pair,
			// redteam two vulnerable targets, and the guard must hold for all
			// of them.
			for sname, svc := range doc.Services {
				_ = sname

				// Egress-free: no network at all, or an internal-only one.
				switch {
				case svc.NetworkMode == "none":
					// The strongest form: the container has no interface.
				case svc.NetworkMode == "":
					if len(svc.Networks) == 0 {
						t.Errorf("%s/%s joins the default network: declare network_mode: none or an internal-only network", name, sname)
					}
					for net := range svc.Networks {
						declared, found := doc.Networks[net]
						if !found || !declared.Internal {
							t.Errorf("%s/%s joins network %q, which is not declared internal-only", name, sname, net)
						}
					}
				default:
					t.Errorf("%s/%s network_mode = %q, want none (or an internal-only network)", name, sname, svc.NetworkMode)
				}

				// Nothing is reachable from outside the container.
				if len(svc.Ports) != 0 {
					t.Errorf("%s/%s publishes ports: %v", name, sname, svc.Ports)
				}
				if len(svc.Expose) != 0 {
					t.Errorf("%s/%s exposes ports: %v", name, sname, svc.Expose)
				}
			}
			for net, declared := range doc.Networks {
				if !declared.Internal {
					t.Errorf("network %q is not internal-only", net)
				}
			}
			for sname, svc := range doc.Services {
				// No host escape hatches, and nothing that would make a service
				// survive the run.
				if svc.Privileged {
					t.Errorf("%s/%s is privileged", name, sname)
				}
				if len(svc.CapAdd) != 0 {
					t.Errorf("%s/%s adds capabilities: %v", name, sname, svc.CapAdd)
				}
				if !contains(svc.CapDrop, "ALL") {
					t.Errorf("%s/%s does not drop every capability: %v", name, sname, svc.CapDrop)
				}
				// read_only is enforced on the blueprint's own fixture services
				// (c2/recon's inert singletons, detection's pair); the public
				// training images redteam runs may need writable roots to be
				// exploitable at all, and that is their point.
				if name != "redteam" && !svc.ReadOnly {
					t.Errorf("%s/%s has a writable root filesystem", name, sname)
				}
			}
			for _, banned := range []string{"network_mode: host", "pid: host", "privileged: true", "cap_add", "restart:"} {
				if strings.Contains(body, banned) {
					t.Errorf("the compose file contains %q", banned)
				}
			}

			// The header says plainly what this is. A reader who opens the file
			// first must not have to infer the posture from the keys.
			head := headerComment(body)
			for _, want := range []string{"synthetic", "egress-free"} {
				if !strings.Contains(strings.ToLower(head), want) {
					t.Errorf("the header comment does not say the lab is %s:\n%s", want, head)
				}
			}
			// And it does not overclaim: no assertion that anything here is a
			// real capability, and no weaponized content.
			if strings.Contains(strings.ToLower(body), "malware") && !strings.Contains(strings.ToLower(body), "no") {
				t.Error("the compose file mentions malware without denying it")
			}
			for sname, svc := range doc.Services {
				if svc.Image == "" {
					t.Errorf("%s/%s names no image", name, sname)
				}
			}
		})
	}
}

// TestLabFromConfigResolvesTheBlueprintComposeFile proves the two halves meet:
// the lab's own defaults, applied to the config a blueprint writes, resolve to
// the compose file that blueprint rendered.
func TestLabFromConfigResolvesTheBlueprintComposeFile(t *testing.T) {
	for _, name := range blueprints.Names() {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if _, _, err := blueprints.Render(name, dir, "demo", false); err != nil {
				t.Fatalf("Render: %v", err)
			}

			// The blueprint writes no [lab] block, so the harness defaults are
			// what a fresh workspace runs on.
			cfg := lab.FromConfig(config.Defaults().Lab)
			if cfg.ComposeFile != lab.DefaultComposeFile {
				t.Fatalf("compose file = %q, want %q", cfg.ComposeFile, lab.DefaultComposeFile)
			}
			if cfg.Project != lab.DefaultProject {
				t.Fatalf("project = %q, want %q", cfg.Project, lab.DefaultProject)
			}

			got := lab.ComposePath(dir, cfg.ComposeFile)
			want := filepath.Join(dir, filepath.FromSlash(lab.DefaultComposeFile))
			if got != want {
				t.Fatalf("ComposePath = %q, want %q", got, want)
			}
			if st, err := os.Stat(got); err != nil || st.Size() == 0 {
				t.Fatalf("the resolved compose file is not the rendered one: %v", err)
			}

			// The workspace's own kintsugi.toml agrees: loading it changes
			// nothing about the lab the harness will drive.
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("KINTSUGI_CONFIG", "")
			loaded, _, err := config.Load(dir)
			if err != nil {
				t.Fatalf("config.Load: %v", err)
			}
			// (Config holds a slice, so it is compared field by field.)
			fromWorkspace := lab.FromConfig(loaded.Lab)
			if fromWorkspace.Runtime != cfg.Runtime ||
				fromWorkspace.ComposeFile != cfg.ComposeFile ||
				fromWorkspace.Project != cfg.Project ||
				fromWorkspace.Network != cfg.Network ||
				len(fromWorkspace.Services) != len(cfg.Services) {
				t.Fatalf("the workspace config resolves a different lab: %+v vs %+v", fromWorkspace, cfg)
			}
		})
	}
}

// TestLabUpAndDownOnARenderedWorkspace is the end of the chain that M8-A
// exists for: `kintsugi new <bp>` then `kintsugi lab up` then `kintsugi lab down`, with
// a FAKE runtime on PATH so no container is ever started and the assertions are
// about the harness (argv, lock) and not about docker.
func TestLabUpAndDownOnARenderedWorkspace(t *testing.T) {
	for _, name := range blueprints.Names() {
		t.Run(name, func(t *testing.T) {
			log := fakeRuntime(t, "docker")
			ws := t.TempDir()
			if _, _, err := blueprints.Render(name, ws, "demo", false); err != nil {
				t.Fatalf("Render: %v", err)
			}

			cfg := lab.FromConfig(config.Defaults().Lab)

			// Nothing is up yet, and the harness says so.
			before, err := lab.Status(context.Background(), ws, cfg)
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if before.Up || before.Locked {
				t.Fatalf("a fresh workspace reports an up lab: %+v", before)
			}

			state, err := lab.Up(context.Background(), ws, cfg)
			if err != nil {
				t.Fatalf("Up on a rendered %s workspace: %v", name, err)
			}
			if !state.Up || !state.Locked {
				t.Fatalf("Up reported %+v, want an up, locked lab", state)
			}
			if state.Runtime != lab.RuntimeDocker || state.Project != lab.DefaultProject {
				t.Fatalf("Up reported %+v, want the docker project %q", state, lab.DefaultProject)
			}

			// The lock exists and names this workspace's compose file.
			lock, err := lab.ReadLock(ws)
			if err != nil {
				t.Fatalf("ReadLock: %v", err)
			}
			if lock == nil {
				t.Fatal("Up wrote no lock")
			}
			if lock.ComposeFile != lab.DefaultComposeFile {
				t.Fatalf("the lock names compose file %q, want %q", lock.ComposeFile, lab.DefaultComposeFile)
			}
			if !lab.SameWorkspace(lock.Workspace, ws) {
				t.Fatalf("the lock names workspace %q, want %q", lock.Workspace, ws)
			}
			if _, err := os.Stat(lab.LockPath(ws)); err != nil {
				t.Fatalf("the lock is not at %s: %v", lab.LockPath(ws), err)
			}

			// The compose client was pointed at the rendered file — the
			// blueprint's compose file, not one the harness invented.
			calls := readCalls(t, log)
			wantUp := "compose -p " + lab.DefaultProject + " -f " +
				filepath.Join(ws, filepath.FromSlash(lab.DefaultComposeFile)) + " up -d"
			if !contains(calls, wantUp) {
				t.Fatalf("the recorded argv has no %q:\n%s", wantUp, strings.Join(calls, "\n"))
			}

			// Status now reports the running lab, from the lock alone.
			during, err := lab.Status(context.Background(), ws, cfg)
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if !during.Up || !during.Locked || during.StartedAt.IsZero() {
				t.Fatalf("Status reported %+v, want an up, locked lab with a start time", during)
			}

			// Down tears it down and lifts the boundary.
			rep, err := lab.DownReport(context.Background(), ws, cfg)
			if err != nil {
				t.Fatalf("Down: %v", err)
			}
			if !rep.LockFound || !rep.LockRemoved {
				t.Fatalf("the teardown report is %+v, want the lock found and removed", rep)
			}
			if !rep.Teardown {
				t.Fatalf("the compose project was not torn down (skip: %q)", rep.SkipReason)
			}
			if _, err := os.Stat(lab.LockPath(ws)); !os.IsNotExist(err) {
				t.Fatalf("the lock survived the teardown: %v", err)
			}
			if !contains(readCalls(t, log), "compose -p "+lab.DefaultProject+" -f "+
				filepath.Join(ws, filepath.FromSlash(lab.DefaultComposeFile))+" down -v") {
				t.Fatalf("the teardown did not run compose down -v:\n%s", strings.Join(readCalls(t, log), "\n"))
			}

			after, err := lab.Status(context.Background(), ws, cfg)
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if after.Up || after.Locked {
				t.Fatalf("the lab is still up after the teardown: %+v", after)
			}
		})
	}
}

// fakeRuntime installs a fake container runtime on PATH and returns the path of
// the file it records its argv in.
//
// The fake answers `compose version` (which is what lab.Detect asks) and exits
// zero for everything else, so the harness's own behaviour is what the tests
// check — never a container runtime's. It is prepended to PATH rather than
// replacing it, so the rest of the test process keeps a usable PATH.
func fakeRuntime(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"log=\"${0%/*}/calls.log\"\n" +
		"printf '%s\\n' \"$*\" >> \"$log\"\n" +
		"if [ \"$1\" = \"compose\" ] && [ \"$2\" = \"version\" ]; then\n" +
		"  echo \"Docker Compose version v2.29.0\"\n" +
		"fi\n" +
		"exit 0\n"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return filepath.Join(dir, "calls.log")
}

// readCalls returns the argv lines the fake runtime recorded, one per call.
func readCalls(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// parseCompose decodes the compose file. A file the harness cannot parse is a
// file the operator's compose client cannot parse either, so this is the first
// real check the blueprint's lab passes.
func parseCompose(t *testing.T, body string) composeDoc {
	t.Helper()
	var doc composeDoc
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("%s is not valid YAML: %v\n%s", lab.DefaultComposeFile, err, body)
	}
	if len(doc.Services) == 0 {
		t.Fatalf("%s declares no services:\n%s", lab.DefaultComposeFile, body)
	}
	return doc
}

// headerComment returns the leading run of `#` lines: the part of the file a
// reader sees before any key.
func headerComment(body string) string {
	var b strings.Builder
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "#") {
			break
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestRenderNoClobberAndForce(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := blueprints.Render("c2", dir, "demo", false); err != nil {
		t.Fatalf("Render: %v", err)
	}

	// An operator edits the scaffold: their words must survive a re-render.
	contextPath := filepath.Join(dir, blueprints.ContextFile)
	const operatorText = "# Project: demo\n\n## Authorization & scope\n\n- mine\n"
	if err := os.WriteFile(contextPath, []byte(operatorText), 0o644); err != nil {
		t.Fatal(err)
	}

	written, skipped, err := blueprints.Render("c2", dir, "demo", false)
	if err != nil {
		t.Fatalf("Render second pass: %v", err)
	}
	if len(written) != 0 {
		t.Errorf("second pass written = %v, want none", written)
	}
	if strings.Join(skipped, ",") != strings.Join(scaffolded, ",") {
		t.Errorf("second pass skipped = %v, want %v", skipped, scaffolded)
	}
	if got := readFile(t, contextPath); got != operatorText {
		t.Errorf("existing CONTEXT.md was modified without force:\n%s", got)
	}

	// --force rewrites everything.
	written, skipped, err = blueprints.Render("c2", dir, "demo", true)
	if err != nil {
		t.Fatalf("Render(force): %v", err)
	}
	if len(skipped) != 0 {
		t.Errorf("force skipped = %v, want none", skipped)
	}
	if strings.Join(written, ",") != strings.Join(scaffolded, ",") {
		t.Errorf("force written = %v, want %v", written, scaffolded)
	}
	if got := readFile(t, contextPath); got == operatorText {
		t.Error("force did not overwrite CONTEXT.md")
	}
}

func TestRenderUnknownBlueprint(t *testing.T) {
	dir := t.TempDir()
	_, _, err := blueprints.Render("listener", dir, "demo", false)
	if err == nil {
		t.Fatal("Render(\"listener\") succeeded; blueprints are capped at c2 and recon")
	}
	msg := err.Error()
	for _, want := range []string{"unknown blueprint", `"listener"`, "c2", "recon"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
	// Nothing was written for an unknown blueprint.
	entries, statErr := os.ReadDir(dir)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if len(entries) != 0 {
		t.Errorf("unknown blueprint wrote %d entries", len(entries))
	}
}

func TestRenderCreatesNestedDir(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "engagement", "lab", "tasking")
	written, skipped, err := blueprints.Render("recon", dir, "nested-project", false)
	if err != nil {
		t.Fatalf("Render into a non-existent nested dir: %v", err)
	}
	if len(skipped) != 0 {
		t.Errorf("skipped = %v, want none", skipped)
	}
	if strings.Join(written, ",") != strings.Join(scaffolded, ",") {
		t.Errorf("written = %v, want %v", written, scaffolded)
	}
	if _, err := os.Stat(filepath.Join(dir, "fixtures", "README.md")); err != nil {
		t.Fatalf("nested fixtures/README.md missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "lab", "docker-compose.yml")); err != nil {
		t.Fatalf("nested lab/docker-compose.yml missing: %v", err)
	}
}

func TestRenderProjectNameDefaultsToDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "my-listeners")
	if _, _, err := blueprints.Render("c2", dir, "", false); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if body := readFile(t, filepath.Join(dir, blueprints.ContextFile)); !strings.HasPrefix(body, "# Project: my-listeners\n") {
		t.Fatalf("empty project name did not default to the dir name:\n%s", firstLine(body))
	}
}

func TestRenderRequiresTarget(t *testing.T) {
	if _, _, err := blueprints.Render("c2", "", "demo", false); err == nil {
		t.Fatal("Render with no target dir should error")
	}
	if _, _, err := blueprints.Render("c2", "   ", "demo", false); err == nil {
		t.Fatal("Render with a blank target dir should error")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[:i]
	}
	return s
}
