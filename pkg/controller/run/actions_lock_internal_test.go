package run

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	lockfile "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/spf13/afero"
	"github.com/suzuki-shunsuke/go-error-with-exit-code/ecerror"
	"github.com/suzuki-shunsuke/pinact/v5/pkg/config"
	"github.com/suzuki-shunsuke/pinact/v5/pkg/github"
)

const lockedSHA = "11bd71901bbe5b1630ceea73d27597364c9af683"

const testActionsLock = `version: v0.0.3
workflows:
  .github/workflows/ci.yml:
    - actions/checkout@v4.2.2
dependencies:
  actions/checkout@v4.2.2:
    ref: v4.2.2
    commit: sha1-` + lockedSHA + `
    owner_id: 44036562
    repo_id: 197814629
`

func lockWorkflow(uses string) string {
	return "name: ci\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: " + uses + "\n"
}

func lockController(t *testing.T, content, workflow string) (*Controller, []string) {
	t.Helper()
	root := t.TempDir()
	files := []string{filepath.Join(root, ".github/workflows/ci.yml")}
	if err := os.MkdirAll(filepath.Dir(files[0]), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(files[0], []byte(workflow), 0o600); err != nil {
		t.Fatal(err)
	}
	if content != "" {
		if err := os.WriteFile(filepath.Join(root, lockfile.Path), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return New(nil, nil, afero.NewOsFs(), &config.Config{}, &ParamRun{
		CWD: root, WorkflowFilePaths: files, ActionsLock: true, NoAPI: true,
		Stderr: &bytes.Buffer{}, Stdout: &bytes.Buffer{},
	}), files
}

func TestActionsLockCoverage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, content, uses, want string
	}{
		{"valid", testActionsLock, "actions/checkout@v4.2.2", ""},
		{"v002", strings.ReplaceAll(testActionsLock, "v0.0.3", "v0.0.2"), "actions/checkout@v4.2.2", ""},
		{"owner case", testActionsLock, "Actions/Checkout@v4.2.2", ""},
		{"subpath shares tree", testActionsLock, "actions/checkout/subpath@v4.2.2", ""},
		{"inline SHA preserved", testActionsLock, "actions/checkout@" + lockedSHA + " # v4.2.2", ""},
		{"different ref", testActionsLock, "actions/checkout@v4", "no matching entry"},
		{"different subpath ref", testActionsLock, "actions/checkout/subpath@v3", "no matching entry"},
		{"ref case", testActionsLock, "actions/checkout@V4.2.2", "no matching entry"},
		{"different repo", testActionsLock, "actions/setup-go@v4.2.2", "no matching entry"},
		{"SHA mismatch", testActionsLock, "actions/checkout@" + strings.Repeat("a", 40), "no matching entry"},
		{"missing dependency", strings.Split(testActionsLock, "dependencies:")[0] + "dependencies: {}\n", "actions/checkout@v4.2.2", "missing"},
		{"invalid commit", strings.ReplaceAll(testActionsLock, lockedSHA, "bad"), "actions/checkout@v4.2.2", "invalid actions.lock"},
		{"nonhex commit", strings.ReplaceAll(testActionsLock, lockedSHA, strings.Repeat("z", 40)), "actions/checkout@v4.2.2", "invalid actions.lock"},
		{"body ref mismatch", strings.ReplaceAll(testActionsLock, "ref: v4.2.2", "ref: v3"), "actions/checkout@v4.2.2", "invalid actions.lock"},
		{"missing identity", strings.ReplaceAll(testActionsLock, "    owner_id: 44036562\n", ""), "actions/checkout@v4.2.2", "invalid actions.lock"},
		{"zero identity", strings.ReplaceAll(testActionsLock, "repo_id: 197814629", "repo_id: 0"), "actions/checkout@v4.2.2", "invalid actions.lock"},
		{"unknown version", strings.ReplaceAll(testActionsLock, "v0.0.3", "v0.0.4"), "actions/checkout@v4.2.2", "invalid actions.lock"},
		{"legacy version", strings.ReplaceAll(testActionsLock, "v0.0.3", "v0.0.1"), "actions/checkout@v4.2.2", "invalid actions.lock"},
		{"unknown field", testActionsLock + "unknown: true\n", "actions/checkout@v4.2.2", "invalid actions.lock"},
		{"wrong shape", "version: v0.0.3\nworkflows: []\ndependencies: []\n", "actions/checkout@v4.2.2", "invalid actions.lock"},
		{"stale direct pin", testActionsLock, "", "not used"},
		{"local action", testActionsLock, "./local-action", "not supported"},
		{"self action", testActionsLock, "$/local-action", "not supported"},
		{"docker", testActionsLock, "docker://alpine:3", "not covered"},
		{"expression", testActionsLock, "actions/checkout@${{ inputs.ref }}", "unsupported uses"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, files := lockController(t, tt.content, lockWorkflow(tt.uses))
			called := false
			c.verifyActionsLock = func(context.Context, string, []string) error {
				called = true
				return errors.New("unexpected network verification")
			}
			_, locked, err := c.prepareActionsLock(t.Context(), slog.New(slog.DiscardHandler), files)
			if tt.want == "" {
				if err != nil || !locked[files[0]] {
					t.Fatalf("coverage = %v, err = %v", locked, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			if called {
				t.Fatal("offline verification invoked the external verifier")
			}
		})
	}
}

func TestActionsLockGraphAndReusableBoundary(t *testing.T) { //nolint:cyclop // Composite closure checks and separate reusable workflow contexts.
	t.Parallel()
	child := "actions/cache@v4"
	content := testActionsLock + "    uses:\n      - " + child + "\n  " + child + ":\n    ref: v4\n    commit: sha1-" + strings.Repeat("a", 40) + "\n    owner_id: 44036562\n    repo_id: 251882839\n"
	for _, tc := range []struct{ name, content, want string }{
		{"recorded composite graph", content, ""},
		{"missing transitive", testActionsLock + "    uses:\n      - " + child + "\n", "missing"},
		{"cycle", testActionsLock + "    uses:\n      - actions/checkout@v4.2.2\n", "cycle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, files := lockController(t, tc.content, lockWorkflow("actions/checkout@v4.2.2"))
			_, _, err := c.prepareActionsLock(t.Context(), slog.New(slog.DiscardHandler), files)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
	// The docs describe caller pinning, but the pinned official verifier does
	// not scan remote reusable workflow refs. Fail closed instead of claiming
	// to authenticate their pins or the callee's internal dependencies.
	remote := strings.ReplaceAll(testActionsLock, "actions/checkout", "octo-org/shared")
	wf := "on: push\njobs:\n  call:\n    uses: octo-org/shared/.github/workflows/reusable.yml@v4.2.2\n"
	c, files := lockController(t, remote, wf)
	if _, _, err := c.prepareActionsLock(t.Context(), slog.New(slog.DiscardHandler), files); err == nil || !strings.Contains(err.Error(), "remote reusable workflow") {
		t.Fatalf("unsupported reusable workflow error = %v", err)
	}
	for _, ref := range []string{"./.github/workflows/reusable.yml", "$/.github/workflows/reusable.yml"} {
		lf := lockfile.File{Workflows: map[string][]string{"ci": {}}}
		if err := validateLockWorkflow([]byte("jobs:\n  call:\n    uses: "+ref+"\n"), lf, "ci"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestActionsLockRunModesAndMixedFiles(t *testing.T) { //nolint:cyclop,gocognit // Exercises mixed workflow, README and composite targets in both write modes.
	t.Parallel()
	for _, fix := range []bool{false, true} {
		t.Run(map[bool]string{false: "check", true: "normal fix"}[fix], func(t *testing.T) {
			t.Parallel()
			c, files := lockController(t, testActionsLock, lockWorkflow("actions/checkout@v4.2.2"))
			c.param.Fix = fix
			// Unregistered workflows and README examples retain inline behavior.
			for _, path := range []string{".github/workflows/other.yml", "README.md", ".github/actions/demo/action.yml"} {
				file := filepath.Join(c.param.CWD, path)
				if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, []byte(lockWorkflow("actions/checkout@"+lockedSHA+" # v4.2.2")), 0o600); err != nil {
					t.Fatal(err)
				}
				files = append(files, file)
			}
			c.param.WorkflowFilePaths = files
			if err := c.Run(t.Context(), slog.New(slog.DiscardHandler)); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(files[0])
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != lockWorkflow("actions/checkout@v4.2.2") {
				t.Fatal("locked workflow changed")
			}
			data, err = os.ReadFile(filepath.Join(c.param.CWD, lockfile.Path))
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != testActionsLock {
				t.Fatal("lockfile changed")
			}
		})
	}
	c, _ := lockController(t, testActionsLock, lockWorkflow("actions/checkout@v4.2.2"))
	c.param.ActionsLock = false
	if err := c.Run(t.Context(), slog.New(slog.DiscardHandler)); ecerror.GetExitCode(err) != ExitCodeUnfixable {
		t.Fatalf("without opt-in, error = %v", err)
	}
	c, _ = lockController(t, "", lockWorkflow("actions/checkout@"+lockedSHA+" # v4.2.2"))
	if err := c.Run(t.Context(), slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	c, files := lockController(t, strings.ReplaceAll(testActionsLock, "ci.yml", "other.yml"), lockWorkflow("actions/checkout@v4.2.2"))
	if _, locked, err := c.prepareActionsLock(t.Context(), slog.New(slog.DiscardHandler), files); err != nil || locked[files[0]] {
		t.Fatalf("unregistered coverage = %v, err = %v", locked, err)
	}
}

func TestActionsLockUnsupportedOptionsBeforeWrites(t *testing.T) {
	t.Parallel()
	for name, setup := range map[string]func(*Controller){
		"update":        func(c *Controller) { c.param.Update = true },
		"comment":       func(c *Controller) { c.param.IsVerify = true },
		"age":           func(c *Controller) { c.param.VerifyMinAge = true },
		"age threshold": func(c *Controller) { c.param.MinAge = 7 },
		"include":       func(c *Controller) { c.param.Includes = []*regexp.Regexp{regexp.MustCompile("actions/")} },
		"exclude":       func(c *Controller) { c.param.Excludes = []*regexp.Regexp{regexp.MustCompile("actions/")} },
		"branch":        func(c *Controller) { c.param.BranchToTags = []*regexp.Regexp{regexp.MustCompile("main")} },
		"sarif":         func(c *Controller) { c.param.Format = "sarif" },
		"diff":          func(c *Controller) { c.param.DiffFilter = &DiffFilter{} },
		"ghes":          func(c *Controller) { c.cfg.GHES = &config.GHES{APIURL: "https://ghes.example.com"} },
		"config age":    func(c *Controller) { c.cfg.MinAge = &config.MinAge{Always: new(true)} },
		"rules":         func(c *Controller) { c.cfg.Rules = []*config.Rule{{Ignore: new(true)}} },
		"ignore":        func(c *Controller) { c.cfg.IgnoreActions = []*config.IgnoreAction{{Name: "actions/.*"}} },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, files := lockController(t, testActionsLock, lockWorkflow("actions/checkout@v4.2.2"))
			c.param.Fix = true
			other := filepath.Join(c.param.CWD, ".github/workflows/aaa.yml")
			if err := os.WriteFile(other, []byte(lockWorkflow("actions/checkout@v4")), 0o600); err != nil {
				t.Fatal(err)
			}
			c.param.WorkflowFilePaths = append([]string{other}, files...)
			setup(c)
			if err := c.Run(t.Context(), slog.New(slog.DiscardHandler)); err == nil {
				t.Fatal("unsupported combination succeeded")
			}
			data, err := os.ReadFile(other)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != lockWorkflow("actions/checkout@v4") {
				t.Fatal("partial update")
			}
		})
	}
}

func TestActionsLockOnlineFailureBeforeWrites(t *testing.T) {
	t.Parallel()
	for _, detail := range []string{"repository identity changed", "ref not in history", "moved full version tag", "gh not installed"} {
		t.Run(detail, func(t *testing.T) {
			t.Parallel()
			c, files := lockController(t, testActionsLock, lockWorkflow("actions/checkout@v4.2.2"))
			c.param.NoAPI, c.param.Fix = false, true
			called := false
			c.verifyActionsLock = func(_ context.Context, root string, workflows []string) error {
				called = true
				if root != c.param.CWD || len(workflows) != 1 || workflows[0] != ".github/workflows/ci.yml" {
					t.Fatal("incorrect verification scope")
				}
				return errors.New(detail)
			}
			if err := c.Run(t.Context(), slog.New(slog.DiscardHandler)); err == nil || !strings.Contains(err.Error(), detail) {
				t.Fatalf("error = %v", err)
			}
			if !called {
				t.Fatal("online verifier not called")
			}
			data, err := os.ReadFile(files[0])
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != lockWorkflow("actions/checkout@v4.2.2") {
				t.Fatal("verification failure changed workflow")
			}
		})
	}
}

func TestActionsLockExplicitAndConfiguredTargets(t *testing.T) {
	t.Parallel()
	c, files := lockController(t, testActionsLock, lockWorkflow("actions/checkout@v4.2.2"))
	c.param.WorkflowFilePaths = []string{filepath.Join(c.param.CWD, lockfile.Path)}
	if err := c.Run(t.Context(), slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	c.param.WorkflowFilePaths = nil
	c.param.ConfigFilePath = filepath.Join(c.param.CWD, ".pinact.yaml")
	c.cfg.Files = []*config.File{{Pattern: ".github/workflows/ci.yml"}}
	if err := c.Run(t.Context(), slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	c.param.WorkflowFilePaths = []string{filepath.Join(c.param.CWD, lockfile.Path), ".github/workflows/ci.yml", files[0]}
	c.param.NoAPI = false
	c.verifyActionsLock = func(_ context.Context, _ string, workflows []string) error {
		if !slices.Equal(workflows, []string{".github/workflows/ci.yml"}) {
			t.Fatalf("duplicate verification scope: %v", workflows)
		}
		return nil
	}
	if err := c.Run(t.Context(), slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
}

func TestActionsLockUnsupportedHost(t *testing.T) {
	c, _ := lockController(t, testActionsLock, lockWorkflow("actions/checkout@v4.2.2"))
	t.Setenv("GITHUB_SERVER_URL", "https://ghes.example.com")
	if err := c.Run(t.Context(), slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("unsupported host accepted")
	}
}

func TestActionsLockOfficialProducerFixture(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile("../../../testdata/actions-lock/actions.lock")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../../../testdata/actions-lock/workflow.yml")
	if err != nil {
		t.Fatal(err)
	}
	lf, err := lockfile.Parse(content)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateLockWorkflow(data, lf, ".github/workflows/workflow.yml"); err != nil {
		t.Fatal(err)
	}
}

func TestActionsLockVerifyCommand(t *testing.T) {
	t.Setenv("GH_ACTIONS_LOCK_INSECURE", "1")
	t.Setenv("GH_ACTIONS_LOCK_WORKFLOWS_DIR", "/another-lockfile")
	t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
	t.Setenv("Gh_Actions_Lock_Insecure", "1")
	t.Setenv("gH_aCtIoNs_LoCk_PrOxY", "https://proxy.example")
	c, _ := lockController(t, testActionsLock, lockWorkflow("actions/checkout@v4.2.2"))
	cmd := c.actionsLockVerifyCommand(t.Context(), c.param.CWD, []string{".github/workflows/ci.yml"})
	want := []string{"gh", "actions-lock", "--verify", "--no-fix", "--no-interactive", "--hostname", "github.com", "--json=valid,findings,workflows", "--", ".github/workflows/ci.yml"}
	if !slices.Equal(cmd.Args, want) || cmd.Dir != c.param.CWD {
		t.Fatalf("command args = %v, dir = %s", cmd.Args, cmd.Dir)
	}
	for _, env := range cmd.Env {
		key, _, _ := strings.Cut(env, "=")
		if strings.HasPrefix(strings.ToUpper(key), "GH_ACTIONS_LOCK_") && env != "GH_ACTIONS_LOCK_REPO_ROOT="+c.param.CWD {
			t.Fatalf("unsafe override retained: %s", env)
		}
	}
	for _, env := range []string{"GH_ACTIONS_LOCK_REPO_ROOT=" + c.param.CWD, "GH_NO_UPDATE_NOTIFIER=1", "GH_NO_EXTENSION_UPDATE_NOTIFIER=1"} {
		if !slices.Contains(cmd.Env, env) {
			t.Fatalf("missing command environment %s", env)
		}
	}
}

func TestActionsLockMixedFixStillPinsUnregistered(t *testing.T) { //nolint:cyclop // Compare mixed fix to legacy fix with deterministic API fixtures.
	t.Parallel()
	for _, optIn := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "mixed lockfile"}[optIn], func(t *testing.T) {
			t.Parallel()
			c, files := lockController(t, testActionsLock, lockWorkflow("actions/checkout@v4.2.2"))
			c.param.ActionsLock, c.param.NoAPI, c.param.Fix = optIn, false, true
			c.cfg.Separator = " # "
			c.verifyActionsLock = func(context.Context, string, []string) error { return nil }
			c.repositoriesService = &github.RepositoriesServiceImpl{Commits: map[string]*github.GetCommitSHA1Result{"actions/checkout/v4.2.2": {SHA: lockedSHA}}}
			other := filepath.Join(c.param.CWD, ".github/workflows/other.yml")
			if err := os.WriteFile(other, []byte(lockWorkflow("actions/checkout@v4.2.2")), 0o600); err != nil {
				t.Fatal(err)
			}
			files = append(files, other)
			c.param.WorkflowFilePaths = files
			if err := c.Run(t.Context(), slog.New(slog.DiscardHandler)); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(other)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), "actions/checkout@"+lockedSHA+" # v4.2.2") {
				t.Fatalf("unregistered workflow was not pinned: %s", data)
			}
			data, err = os.ReadFile(files[0])
			if err != nil {
				t.Fatal(err)
			}
			if optIn && string(data) != lockWorkflow("actions/checkout@v4.2.2") {
				t.Fatal("registered workflow was rewritten")
			}
			if !optIn && !strings.Contains(string(data), "actions/checkout@"+lockedSHA) {
				t.Fatal("legacy SHA pinning changed")
			}
			data, err = os.ReadFile(filepath.Join(c.param.CWD, lockfile.Path))
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != testActionsLock {
				t.Fatal("lockfile was rewritten")
			}
		})
	}
}

func TestActionsLockFrozenReport(t *testing.T) {
	t.Parallel()
	good := `{"valid":true,"findings":[],"workflows":[{"path":".github/workflows/ci.yml","valid":true}]}`
	for _, tc := range []struct {
		name, json string
		fail       bool
	}{
		{"valid", good, false},
		{"repo replaced", strings.Replace(good, `"findings":[]`, `"findings":[{"category":"repo-replaced","detail":"different repo ID"}]`, 1), true},
		{"owner redirect", strings.Replace(good, `"findings":[]`, `"findings":[{"category":"repo-renamed","severity":"warning"}]`, 1), true},
		{"invalid history", strings.Replace(good, `"findings":[]`, `"findings":[{"category":"unreachable-pin"}]`, 1), true},
		{"tag moved", strings.Replace(good, `"findings":[]`, `"findings":[{"category":"ref-moved"}]`, 1), true},
		{"inconclusive zero exit", strings.Replace(good, `"findings":[]`, `"findings":[{"category":"reachability-unknown","severity":"warning"}]`, 1), true},
		{"SHA alias not verified", strings.Replace(good, `"findings":[]`, `"findings":[{"category":"sha-as-ref","severity":"warning"}]`, 1), true},
		{"future finding", strings.Replace(good, `"findings":[]`, `"findings":[{"category":"future-warning"}]`, 1), true},
		{"invalid", strings.Replace(good, `"valid":true`, `"valid":false`, 1), true},
		{"missing findings", `{"valid":true}`, true},
		{"null findings", strings.Replace(good, `"findings":[]`, `"findings":null`, 1), true},
		{"unreadable", `not JSON`, true},
		{"wrong scope", strings.Replace(good, "ci.yml", "other.yml", 1), true},
		{"empty scope", `{"valid":true,"findings":[],"workflows":[]}`, true},
		{"unverified workflow", strings.Replace(good, `"path":".github/workflows/ci.yml","valid":true`, `"path":".github/workflows/ci.yml","valid":false`, 1), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateActionsLockReport([]byte(tc.json), []string{".github/workflows/ci.yml"})
			if (err != nil) != tc.fail {
				t.Fatalf("error = %v, want failure %v", err, tc.fail)
			}
		})
	}
}

func TestActionsLockInvalidWorkflowRegistration(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"README.md", ".github/actions/demo/action.yml", ".github/workflows/nested/ci.yml"} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			c, _ := lockController(t, strings.ReplaceAll(testActionsLock, ".github/workflows/ci.yml", key), lockWorkflow("actions/checkout@v4.2.2"))
			c.param.WorkflowFilePaths = []string{filepath.Join(c.param.CWD, lockfile.Path)}
			if err := c.Run(t.Context(), slog.New(slog.DiscardHandler)); err == nil || !strings.Contains(err.Error(), "invalid actions.lock registration") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
