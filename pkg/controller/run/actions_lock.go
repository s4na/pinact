package run

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	lockfile "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/spf13/afero"
	"github.com/suzuki-shunsuke/go-error-with-exit-code/ecerror"
	"gopkg.in/yaml.v3"
)

const actionsLockHost = "github.com"

func (c *Controller) filesForRun(ctx context.Context, logger *slog.Logger) ([]string, error) {
	files, err := c.searchFiles()
	if err != nil {
		return nil, fmt.Errorf("search target files: %w", err)
	}
	files, locked, err := c.prepareActionsLock(ctx, logger, files)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(files, func(file string) bool { return locked[file] }), nil
}

// prepareActionsLock validates all selected locked workflows before the first
// write, including writes to ordinary workflows in a mixed invocation.
func (c *Controller) prepareActionsLock(ctx context.Context, logger *slog.Logger, files []string) ([]string, map[string]bool, error) {
	locked := map[string]bool{}
	if !c.param.ActionsLock {
		return files, locked, nil
	}
	if c.param.DiffFilter != nil {
		return nil, nil, lockError("--diff-file cannot verify a complete actions.lock; run without --diff-file from the repository root")
	}
	root, lf, err := c.loadActionsLock()
	if errors.Is(err, os.ErrNotExist) {
		return missingActionsLockTargets(root, files)
	}
	if err != nil {
		return nil, nil, err
	}
	files, err = expandLockTarget(root, files, *lf)
	if err != nil {
		return nil, nil, err
	}
	locked, workflows := selectLockedWorkflows(root, files, *lf)
	if len(workflows) == 0 {
		return files, locked, nil
	}
	if err := c.validateActionsLockOptions(); err != nil {
		return nil, nil, err
	}
	if err := c.validateLockedWorkflows(root, *lf, workflows); err != nil {
		return nil, nil, err
	}
	if err := c.verifyLockedWorkflows(ctx, logger, root, workflows); err != nil {
		return nil, nil, err
	}
	return files, locked, nil
}

func missingActionsLockTargets(root string, files []string) ([]string, map[string]bool, error) {
	for _, file := range files {
		if lockWorkflowKey(root, file) == lockfile.Path {
			return nil, nil, lockError("actions.lock does not exist; onboard workflows with gh actions-lock first")
		}
	}
	return files, map[string]bool{}, nil
}

func selectLockedWorkflows(root string, files []string, lf lockfile.File) (map[string]bool, []string) {
	locked := map[string]bool{}
	var workflows []string
	for _, file := range files {
		key := lockWorkflowKey(root, file)
		if !isLockWorkflow(key) {
			continue
		}
		if _, ok := lf.LookupWorkflow(key); ok {
			locked[file] = true
			workflows = append(workflows, key)
		}
	}
	slices.Sort(workflows)
	return locked, slices.Compact(workflows)
}

func (c *Controller) verifyLockedWorkflows(ctx context.Context, logger *slog.Logger, root string, workflows []string) error {
	if c.param.NoAPI {
		logger.Warn("actions.lock: local coverage and recorded graph checked only; repository identity, upstream refs and graph completeness were not verified")
	} else {
		verify := c.verifyActionsLock
		if verify == nil {
			verify = c.runActionsLockVerify
		}
		if err := verify(ctx, root, workflows); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) loadActionsLock() (string, *lockfile.File, error) {
	root, err := filepath.Abs(c.param.CWD)
	if err != nil {
		return "", nil, fmt.Errorf("resolve actions.lock root: %w", err)
	}
	content, err := afero.ReadFile(c.fs, filepath.Join(root, lockfile.Path))
	if err != nil {
		return root, nil, ecerror.Wrap(fmt.Errorf("read actions.lock: %w", err), ExitCodeAPIError)
	}
	lf, err := lockfile.ParseWithPolicy(content, lockfile.VersionPolicy{Min: "v0.0.2", Max: "v0.0.3"})
	if err != nil {
		return root, nil, lockError("invalid actions.lock: %v; inspect it and regenerate with gh actions-lock", err)
	}
	if lf.Workflows == nil || lf.Dependencies == nil {
		return root, nil, lockError("invalid actions.lock: workflows and dependencies must be mappings")
	}
	for workflow := range lf.Workflows {
		if !isLockWorkflow(workflow) {
			return root, nil, lockError("invalid actions.lock registration %q: expected a workflow directly under .github/workflows", workflow)
		}
	}
	return root, &lf, nil
}

func (c *Controller) validateLockedWorkflows(root string, lf lockfile.File, workflows []string) error {
	for _, workflow := range workflows {
		if err := validateLockGraph(lf, lf.Workflows[workflow], map[string]bool{}); err != nil {
			return lockError("%s: %v", workflow, err)
		}
		data, err := afero.ReadFile(c.fs, filepath.Join(root, filepath.FromSlash(workflow)))
		if err != nil {
			return ecerror.Wrap(fmt.Errorf("read locked workflow %s: %w", workflow, err), ExitCodeAPIError)
		}
		if err := validateLockWorkflow(data, lf, workflow); err != nil {
			return lockError("%s: %v; inspect and run gh actions-lock", workflow, err)
		}
	}
	return nil
}

func lockError(format string, args ...any) error {
	return ecerror.Wrap(fmt.Errorf(format, args...), ExitCodeUnfixable)
}

// lockWorkflowKey never grants lockfile coverage to files outside CWD.
func lockWorkflowKey(root, file string) string {
	if !filepath.IsAbs(file) {
		file = filepath.Join(root, file)
	}
	key, err := filepath.Rel(root, file)
	if err != nil {
		return ""
	}
	return filepath.ToSlash(key)
}

func isLockWorkflow(key string) bool {
	return filepath.ToSlash(filepath.Dir(key)) == ".github/workflows" &&
		(filepath.Ext(key) == ".yml" || filepath.Ext(key) == ".yaml")
}

func expandLockTarget(root string, files []string, lf lockfile.File) ([]string, error) {
	var out []string
	for _, file := range files {
		if lockWorkflowKey(root, file) != lockfile.Path {
			out = append(out, file)
			continue
		}
		if len(lf.Workflows) == 0 {
			return nil, lockError("actions.lock registers no workflows; onboard workflows with gh actions-lock first")
		}
		for key := range lf.Workflows {
			if isLockWorkflow(key) {
				out = append(out, filepath.Join(root, filepath.FromSlash(key)))
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

func (c *Controller) validateActionsLockOptions() error {
	p := c.param
	if p.IsGHES {
		return lockError("actions.lock integration does not support the selected GitHub Enterprise API; use inline SHA checking")
	}
	if p.Update {
		return lockError("--update is not supported for locked workflows; use Dependabot or review workflow changes with gh actions-lock (--relock is not a latest-version update)")
	}
	if unsupportedLockFlags(p) {
		return lockError("actions.lock does not support --verify-comment, min-age, action filters, --branch-to-tag or --format; select unregistered files separately, and use gh actions-lock --verify for locked workflows")
	}
	if c.cfg != nil && (c.cfg.GHES != nil || c.cfg.MinAge != nil || len(c.cfg.Rules)+len(c.cfg.IgnoreActions) != 0) {
		return lockError("actions.lock does not support ghes, min_age, rules or ignore_actions configuration; use a separate configuration for locked workflows")
	}
	return validateActionsLockHost()
}

func unsupportedLockFlags(p *ParamRun) bool {
	return p.IsVerify || p.VerifyMinAge || p.MinAge != 0 || len(p.Includes)+len(p.Excludes)+len(p.BranchToTags) != 0 || p.Format != ""
}

func validateActionsLockHost() error {
	// Opt-in attests to runtime enforcement, not merely the presence of a file.
	// Refuse known incompatible execution contexts even with that opt-in.
	for _, name := range []string{"GITHUB_SERVER_URL", "GH_HOST"} {
		if host := os.Getenv(name); host != "" && host != actionsLockHost && host != "https://"+actionsLockHost {
			return lockError("actions.lock integration only supports enforced GitHub.com workflows; %s selects %s", name, host)
		}
	}
	return nil
}

func validateLockGraph(lf lockfile.File, pins []string, seen map[string]bool) error {
	for _, pin := range pins {
		if seen[pin] {
			continue
		}
		seen[pin] = true
		action, ok := lf.Dependencies[pin]
		if !ok {
			return fmt.Errorf("dependency %q is missing from actions.lock", pin)
		}
		if action.Hostname != "" && action.Hostname != actionsLockHost {
			return fmt.Errorf("dependency %q uses unsupported host %q", pin, action.Hostname)
		}
		if err := validateLockGraph(lf, action.Uses, seen); err != nil {
			return err
		}
	}
	return nil
}

// Workflow references are read from YAML jobs/steps, never from README-like
// text matching. Pin keys are repo+ref scoped: subpaths share the repo tree.
func validateLockWorkflow(data []byte, lf lockfile.File, workflow string) error {
	var wf struct {
		Jobs map[string]struct {
			Uses  string `yaml:"uses"`
			Steps []struct {
				Uses string `yaml:"uses"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &wf); err != nil {
		return fmt.Errorf("parse workflow: %w", err)
	}
	used := map[string]bool{}

	for _, job := range wf.Jobs {
		if err := validateLockedUse(job.Uses, true, lf, workflow, used); err != nil {
			return err
		}
		for _, step := range job.Steps {
			if err := validateLockedUse(step.Uses, false, lf, workflow, used); err != nil {
				return err
			}
		}
	}
	// The workflow registration is a flat list containing both direct refs and
	// dependencies discovered inside composite actions. Check that every entry
	// is reachable from a ref in the workflow instead of requiring every entry
	// to appear directly in YAML.
	reachable := map[string]bool{}
	var visit func(string)
	visit = func(pin string) {
		if reachable[pin] {
			return
		}
		reachable[pin] = true
		for _, dependency := range lf.Dependencies[pin].Uses {
			visit(dependency)
		}
	}
	for pin := range used {
		visit(pin)
	}
	for _, pin := range lf.Workflows[workflow] {
		if !reachable[pin] {
			return fmt.Errorf("registered dependency %q is not used by the workflow or its transitive dependencies", pin)
		}
	}
	return nil
}

func validateLockedUse(value string, job bool, lf lockfile.File, workflow string, used map[string]bool) error {
	if value == "" {
		return nil
	}
	if job && lockfile.IsLocalReusableWorkflow(strings.Replace(value, "$/", "./", 1)) && !strings.Contains(value, "@") {
		return nil // Called workflow owns its dependency lockfile context.
	}
	ref, err := remoteLockedRef(value, job)
	if err != nil {
		return err
	}
	if !matchLockedPins(ref, lf, workflow, used) {
		return fmt.Errorf("uses %q has no matching entry in this workflow's actions.lock registration", value)
	}
	return nil
}

func remoteLockedRef(value string, job bool) (*lockfile.ActionRef, error) {
	if strings.HasPrefix(value, "./") || strings.HasPrefix(value, "$/") {
		return nil, fmt.Errorf("local/self repository action %q is not supported by this integration; verify it separately with gh actions-lock", value)
	}
	if strings.HasPrefix(value, "docker://") {
		return nil, fmt.Errorf("container reference %q is not covered by actions.lock; use a separate SHA/digest policy", value)
	}
	if job {
		if reusable := lockfile.ParseReusableWorkflowRef(value); reusable != nil {
			return nil, fmt.Errorf("remote reusable workflow %q is not supported by the current official verifier integration; pin the caller reference inline and verify the callee repository separately", value)
		}
	} else if ref := lockfile.ParseActionRef(value); ref != nil {
		return ref, nil
	}
	return nil, fmt.Errorf("unsupported uses reference %q", value)
}

func matchLockedPins(ref *lockfile.ActionRef, lf lockfile.File, workflow string, used map[string]bool) bool {
	matched := false
	for _, pin := range lf.Workflows[workflow] {
		parsed, ok := lockfile.ParsePin(pin)
		if !ok || parsed.Owner != strings.ToLower(ref.Owner) || parsed.Repo != strings.ToLower(ref.Repo) {
			continue
		}
		_, digest, _ := strings.Cut(lf.Dependencies[pin].Commit, "-")
		if parsed.Ref == ref.Ref || (lockfile.IsFullSha(ref.Ref) && strings.EqualFold(digest, ref.Ref)) {
			used[pin] = true
			matched = true
		}
	}
	return matched
}

func (c *Controller) actionsLockVerifyCommand(ctx context.Context, root string, workflows []string) *exec.Cmd {
	args := append([]string{"actions-lock", "--verify", "--no-fix", "--no-interactive", "--hostname", actionsLockHost, "--json=valid,findings,workflows", "--"}, workflows...)
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = root
	cmd.Stdout, cmd.Stderr = c.param.Stderr, c.param.Stderr
	// Extension overrides can select another lockfile, a proxy, or insecure
	// transport. They cannot alter the verification of the file we just read.
	for _, env := range os.Environ() {
		key, _, _ := strings.Cut(env, "=")
		if !strings.HasPrefix(strings.ToUpper(key), "GH_ACTIONS_LOCK_") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	cmd.Env = append(cmd.Env, "GH_ACTIONS_LOCK_REPO_ROOT="+root, "GH_NO_UPDATE_NOTIFIER=1", "GH_NO_EXTENSION_UPDATE_NOTIFIER=1")
	return cmd
}

func (c *Controller) runActionsLockVerify(ctx context.Context, root string, workflows []string) error {
	cmd := c.actionsLockVerifyCommand(ctx, root, workflows)
	var output bytes.Buffer
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil {
		return lockError("gh actions-lock --verify failed: %v; install gh and github/gh-actions-lock, configure gh authentication separately, and inspect the verification failure", err)
	}
	return validateActionsLockReport(output.Bytes(), workflows)
}

type lockWorkflowReport struct {
	Path  string `json:"path"`
	Valid *bool  `json:"valid"`
}

func validateActionsLockReport(data []byte, workflows []string) error {
	var report struct {
		Valid    *bool `json:"valid"`
		Findings *[]struct {
			Category string `json:"category"`
			Detail   string `json:"detail"`
		} `json:"findings"`
		Workflows *[]lockWorkflowReport `json:"workflows"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		return lockError("gh actions-lock returned an unreadable verification report: %v", err)
	}
	if report.Valid == nil || report.Findings == nil || report.Workflows == nil {
		return lockError("gh actions-lock verification report is incomplete; update the official extension")
	}
	// The official CLI can exit zero for an inconclusive upstream lookup.
	// Any finding, including a warning or future category, fails closed here.
	if len(*report.Findings) != 0 {
		finding := (*report.Findings)[0]
		return lockError("gh actions-lock verification reported %s: %s; inspect all findings with gh actions-lock --verify --json", finding.Category, finding.Detail)
	}
	if !*report.Valid {
		return lockError("gh actions-lock reported an invalid lockfile")
	}
	return validateActionsLockReportScope(workflows, *report.Workflows)
}

func validateActionsLockReportScope(workflows []string, reports []lockWorkflowReport) error {
	seen := map[string]bool{}
	for _, report := range reports {
		if report.Valid == nil || !*report.Valid || seen[report.Path] || !slices.Contains(workflows, report.Path) {
			return lockError("gh actions-lock did not verify the requested workflow scope")
		}
		seen[report.Path] = true
	}
	if len(seen) != len(workflows) {
		return lockError("gh actions-lock did not verify every requested workflow")
	}
	return nil
}
