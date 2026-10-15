package feedback

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/faustbrian/reviewd/internal/clarification"
	"github.com/faustbrian/reviewd/internal/codex"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/review"
	"github.com/faustbrian/reviewd/internal/source"
)

// Execution is the capability broker for one already-authorized feedback run.
// It owns no process/recovery authority: Command and Quiesce use the run's
// common PrivateOwner. Save writes facts into that run's existing journal.
type ExecutionOptions struct {
	Policy       config.Snapshot
	Skill        *review.CapturedSkill
	Snapshot     *source.Snapshot
	Workspace    string
	Environment  CommandEnvironment
	Inspect      func(context.Context) (GitInspection, error)
	Inventory    Inventory
	BeforeWrite  func(context.Context) error
	Documents    map[string]string
	Command      func(context.Context, []string, time.Duration, bool) (codex.CommandResult, error)
	Quiesce      func() error
	Agent        func(context.Context, codex.FeedbackRequest) (codex.ReviewOutput, error)
	Save         func(ExecutionJournal) error
	OnInput      func(context.Context, clarification.Request) (clarification.Answers, error)
	OnInputEvent func(context.Context, clarification.Request, string, string) error
	Deliver      func(context.Context, DeliveryProposal, string, []ItemDisposition, func(DeliveryJournal) error) (DeliveryJournal, error)
	Previous     *ExecutionJournal
	OwnerRoot    string
	Frozen       *executionTree
}

type NativeRoleReport struct {
	Role             string            `json:"role"`
	Tree             string            `json:"tree"`
	ThreadID         string            `json:"thread_id"`
	TurnID           string            `json:"turn_id"`
	RequestedModel   string            `json:"requested_model"`
	EffectiveModel   string            `json:"effective_model"`
	Text             string            `json:"text"`
	Usage            json.RawMessage   `json:"usage,omitempty"`
	Disposition      string            `json:"disposition,omitempty"`
	Error            string            `json:"error,omitempty"`
	Generation       uint64            `json:"generation"`
	AfterDelivery    bool              `json:"after_delivery,omitempty"`
	PlanningBlockers []PlanningBlocker `json:"planning_blockers,omitempty"`
}

// Preimplementation findings are work inputs, not a review of an unfixed bug.
// Only these concrete invalidating conditions can stop the bounded input pass.
type PlanningBlocker struct {
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

func planningRole(role string) bool { return role == "change_mapper" || role == "qa_test_planner" }

type CommandObservation struct {
	Argv       []string `json:"argv"`
	Access     string   `json:"access"`
	ExitCode   int      `json:"exit_code"`
	Stdout     string   `json:"stdout,omitempty"`
	Stderr     string   `json:"stderr,omitempty"`
	Tree       string   `json:"tree"`
	Generation uint64   `json:"generation"`
	Error      string   `json:"error,omitempty"`
}

type ItemDisposition struct {
	Key      string `json:"key"`
	Revision string `json:"revision"`
	ThreadID string `json:"thread_id,omitempty"`
	Status   string `json:"status"`
	Evidence string `json:"evidence"`
}

type ExecutionJournal struct {
	Environment  CommandEnvironment   `json:"environment"`
	Dispositions []ItemDisposition    `json:"dispositions"`
	Roles        []NativeRoleReport   `json:"roles"`
	Commands     []CommandObservation `json:"commands"`
	Generation   uint64               `json:"generation"`
	Tree         string               `json:"tree"`
	Delivery     DeliveryJournal      `json:"delivery"`
}

type Execution struct {
	mu            sync.Mutex
	options       ExecutionOptions
	paths         map[string]bool
	journal       ExecutionJournal
	primaryThread string
	poisoned      error
	lastTree      executionTree
}

func NewExecution(options ExecutionOptions) (*Execution, error) {
	if options.Skill == nil || options.Snapshot == nil || options.Workspace == "" || options.Inspect == nil || options.Command == nil || options.Quiesce == nil || options.Agent == nil || options.Save == nil {
		return nil, errors.New("feedback execution requires captured instructions/source, owned commands, fresh agents and durable journal")
	}
	paths := map[string]bool{}
	for _, entry := range options.Snapshot.Manifests["head"] {
		if entry.Type == "blob" {
			if !executionPath(entry.Path) {
				return nil, errors.New("immutable source contains an unsupported feedback path")
			}
			paths[entry.Path] = true
		}
	}
	journal := ExecutionJournal{Environment: options.Environment, Roles: []NativeRoleReport{}, Commands: []CommandObservation{}}
	if options.Previous != nil {
		journal = cloneJournal(*options.Previous)
		journal.Environment = options.Environment
		if journal.Delivery.Started {
			if options.Frozen == nil || options.Frozen.ID != journal.Tree || journal.Delivery.Candidate != journal.Tree {
				return nil, errors.New("attempted delivery requires its exact original reviewed projection")
			}
		} else {
			journal.Generation++
			journal.Tree = ""
		}
	}
	result := &Execution{options: options, paths: paths, journal: journal}
	if options.Frozen != nil {
		result.lastTree = *options.Frozen
	}
	return result, nil
}

func (e *Execution) SetPrimaryIdentity(thread string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if thread == "" || (e.primaryThread != "" && e.primaryThread != thread) {
		return errors.New("feedback primary execution identity changed")
	}
	for _, role := range e.journal.Roles {
		if role.ThreadID == thread {
			return errors.New("feedback primary reused an independent session")
		}
	}
	e.primaryThread = thread
	return nil
}

func cloneJournal(j ExecutionJournal) ExecutionJournal {
	data, _ := json.Marshal(j)
	var copy ExecutionJournal
	_ = json.Unmarshal(data, &copy)
	return copy
}
func (e *Execution) Journal() ExecutionJournal {
	e.mu.Lock()
	defer e.mu.Unlock()
	return cloneJournal(e.journal)
}

func (e *Execution) recordReconciliation(delivery DeliveryJournal) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.journal.Delivery.Started || !delivery.Started || e.journal.Delivery.Candidate != delivery.Candidate {
		return errors.New("reconciliation cannot replace the frozen delivery candidate")
	}
	e.journal.Delivery = delivery
	return e.persist()
}
func (e *Execution) persist() error {
	if err := e.options.Save(cloneJournal(e.journal)); err != nil {
		e.poisoned = err
		return err
	}
	return nil
}

func (e *Execution) capture(ctx context.Context) (executionTree, error) {
	if e.journal.Delivery.Started {
		return e.lastTree, nil
	}
	if err := e.options.Quiesce(); err != nil {
		e.poisoned = err
		return executionTree{}, err
	}
	inspection, err := e.options.Inspect(ctx)
	if err != nil {
		e.poisoned = err
		return executionTree{}, err
	}
	if err := e.options.Quiesce(); err != nil {
		e.poisoned = err
		return executionTree{}, err
	}
	paths := make([]string, 0, len(e.paths))
	for path := range e.paths {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	tree, err := captureExecutionTree(e.options.Workspace, paths)
	if err != nil {
		e.poisoned = err
		return tree, err
	}
	tree.Git = inspection
	identity, _ := json.Marshal(struct {
		Worktree string
		Git      GitInspection
	}{tree.ID, inspection})
	digest := sha256.Sum256(identity)
	tree.ID = hex.EncodeToString(digest[:])
	if e.journal.Tree != "" && e.journal.Tree != tree.ID {
		e.journal.Generation++
	}
	e.journal.Tree = tree.ID
	e.lastTree = tree
	return tree, nil
}

func feedbackEvidenceTools() []map[string]any {
	tools := codex.EvidenceTools()
	for _, tool := range tools {
		tool["name"] = strings.Replace(tool["name"].(string), "reviewd_", "reviewd_feedback_", 1)
	}
	return tools
}
func feedbackEvidenceHandle(broker *codex.EvidenceBroker) func(context.Context, string, json.RawMessage) (string, error) {
	return func(_ context.Context, name string, args json.RawMessage) (string, error) {
		return broker.Handle(strings.Replace(name, "reviewd_feedback_", "reviewd_", 1), args)
	}
}

func executionTool(name, description string, properties map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "function", "name": "reviewd_feedback_" + name, "description": description, "inputSchema": map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}}
}
func (e *Execution) Tools() []map[string]any {
	text := map[string]any{"type": "string"}
	array := map[string]any{"type": "array", "items": text, "maxItems": 256}
	return append(feedbackEvidenceTools(),
		executionTool("disposition", "Record a source-backed per-item decision for an exact captured human revision. Accepted/already-addressed/rejected/deferred/needs-clarification are decisions, never proof of a delivered fix or permission to resolve.", map[string]any{"key": text, "revision": text, "status": map[string]any{"type": "string", "enum": []string{"accepted", "already_addressed", "rejected", "deferred", "needs_clarification"}}, "evidence": text}, "key", "revision", "status", "evidence"),
		executionTool("command", "Run bounded argv only in this PR's credential-free isolated command environment, identified in execution-environment. macOS uses Linux guest commands, not native macOS checks. Read commands cannot write; write/verify require actual independent mapper and the single QA pass. New paths are discovered independently; declaring them is useful but cannot hide other changes. Every write invalidates prior final reviews. No direct remote capability.", map[string]any{"argv": array, "timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 1200}, "access": map[string]any{"type": "string", "enum": []string{"read", "write", "verify"}}, "paths": array}, "argv", "timeout_seconds", "access"),
		executionTool("delegate", "Start a fresh private, read-only, do-not-post native role. The application supplies authoritative requirements and current complete source, not a parent implementation claim. One QA planning pass; later reviews bind the current tree and verification.", map[string]any{"role": map[string]any{"type": "string", "enum": []string{"change_mapper", "qa_test_planner", "regression_reviewer", "correctness_reviewer", "delivery_auditor", "billing_invariants", "laravel_boundaries", "go_compatibility"}}}, "role"),
		executionTool("deliver", "Propose the existing-branch fix and original feedback responses after current-candidate verification and independent review. First explicitly stage only your changed paths with command access=write and git add; then run final checks and reviews on that staged candidate. The application never stages paths for you. The application runs normal hooks/scoped signing, additive exact-head push and canonically verifies GitHub effects. This freezes commands; uncertain effects remain pending and are never replayed.", map[string]any{"commit_message": text, "title": text, "body": text, "replies": map[string]any{"type": "array", "maxItems": 1000, "items": map[string]any{"type": "object", "properties": map[string]any{"key": text, "body": text, "resolve": map[string]any{"type": "boolean"}}, "required": []string{"key", "body", "resolve"}, "additionalProperties": false}}}, "commit_message", "title", "body", "replies"),
	)
}

func decodeExecution(args json.RawMessage, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(args))
	decoder.DisallowUnknownFields()
	if len(args) > 256<<10 || decoder.Decode(value) != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("feedback request requires one bounded object with only granted fields")
	}
	return nil
}

func (e *Execution) Handle(ctx context.Context, name string, args json.RawMessage) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.poisoned != nil {
		return "", fmt.Errorf("feedback execution stopped; preserve its owner/journal: %w", e.poisoned)
	}
	switch name {
	case "reviewd_feedback_disposition":
		return e.disposition(args)
	case "reviewd_feedback_command":
		return e.command(ctx, args)
	case "reviewd_feedback_delegate":
		return e.delegate(ctx, args)
	case "reviewd_feedback_deliver":
		return e.deliver(ctx, args)
	case "reviewd_feedback_read_source", "reviewd_feedback_list_source", "reviewd_feedback_read_evidence":
		tree, err := e.capture(ctx)
		if err != nil {
			return "", err
		}
		return feedbackEvidenceHandle(e.evidence(tree))(ctx, name, args)
	default:
		return "", errors.New("feedback capability is not granted; no remote delivery was attempted")
	}
}

func (e *Execution) approved(role string, current bool) bool {
	for i := len(e.journal.Roles) - 1; i >= 0; i-- {
		report := e.journal.Roles[i]
		if report.Role == role {
			expected := "APPROVE"
			if planningRole(role) {
				expected = "PLANNING_COMPLETE"
			}
			return report.Error == "" && report.Disposition == expected && (!current || (report.Tree == e.journal.Tree && report.Generation == e.journal.Generation))
		}
	}
	return false
}
func (e *Execution) verified() bool {
	// Every distinct check attempted on this exact candidate must have a known
	// successful latest outcome. Retrying that check may replace its earlier
	// failure; running a different passing check must not erase it.
	seen := map[string]bool{}
	for i := len(e.journal.Commands) - 1; i >= 0; i-- {
		c := e.journal.Commands[i]
		if c.Access == "verify" && c.Tree == e.journal.Tree && c.Generation == e.journal.Generation {
			key := fingerprint(c.Argv)
			if seen[key] {
				continue
			}
			seen[key] = true
			if c.ExitCode != 0 || c.Error != "" {
				return false
			}
		}
	}
	return len(seen) > 0
}
func (e *Execution) deliveryReady() bool {
	return e.poisoned == nil && e.verified() && e.approved("regression_reviewer", true) && e.approved("correctness_reviewer", true) && e.approved("delivery_auditor", true)
}
func (e *Execution) DeliveryReady() bool { e.mu.Lock(); defer e.mu.Unlock(); return e.deliveryReady() }

func (e *Execution) command(ctx context.Context, args json.RawMessage) (string, error) {
	if e.journal.Delivery.Started {
		return "", errors.New("delivery froze this candidate; reconcile its recorded effects before further repository commands")
	}
	var request struct {
		Argv    []string `json:"argv"`
		Timeout int      `json:"timeout_seconds"`
		Access  string   `json:"access"`
		Paths   []string `json:"paths"`
	}
	if err := decodeExecution(args, &request); err != nil {
		return "", err
	}
	if len(request.Argv) == 0 || len(request.Argv) > 256 || request.Timeout < 1 || request.Timeout > 1200 || (request.Access != "read" && request.Access != "write" && request.Access != "verify") {
		return "", errors.New("feedback command requires bounded argv, timeout and explicit read/write/verify access")
	}
	for _, path := range request.Paths {
		if !executionPath(path) {
			return "", errors.New("feedback source path is outside the existing PR workspace")
		}
	}
	if len(request.Paths) > 256 || (request.Access != "write" && len(request.Paths) > 0) {
		return "", errors.New("only modifying commands declare new source paths")
	}
	writable := request.Access != "read"
	if writable && (!e.approved("change_mapper", false) || !e.approved("qa_test_planner", false)) {
		return "", errors.New("source writes require the actual independent mapper and one bounded QA pass first")
	}
	if writable && e.options.BeforeWrite != nil {
		if err := e.options.BeforeWrite(ctx); err != nil {
			return "", err
		}
	}
	if request.Access == "write" {
		for _, path := range request.Paths {
			e.paths[path] = true
		}
		e.journal.Generation++
	}
	tree, err := e.capture(ctx)
	if err != nil {
		return "", err
	}
	observation := CommandObservation{Argv: append([]string{}, request.Argv...), Access: request.Access, Generation: e.journal.Generation, Tree: tree.ID}
	// Persist the attempted command before launch. Unknown/crashed execution is
	// not a passing check and can never authorize delivery.
	observation.Error = "command outcome not yet known"
	e.journal.Commands = append(e.journal.Commands, observation)
	if err := e.persist(); err != nil {
		return "", err
	}
	result, runErr := e.options.Command(ctx, request.Argv, time.Duration(request.Timeout)*time.Second, writable)
	after, captureErr := e.capture(ctx)
	err = errors.Join(runErr, captureErr)
	observation.ExitCode = result.ExitCode
	observation.Stdout, observation.Stderr = result.Stdout, result.Stderr
	observation.Error = ""
	if err != nil {
		observation.Error = err.Error()
	}
	if after.ID != tree.ID && request.Access == "verify" {
		observation.Error = "verification changed source; rerun checks and review the resulting complete tree"
		err = errors.Join(err, errors.New(observation.Error))
	}
	// The original observation identity deliberately stays on the pre-command
	// tree/generation. A test which changed source cannot verify its new result.
	e.journal.Commands[len(e.journal.Commands)-1] = observation
	if saveErr := e.persist(); saveErr != nil {
		return "", errors.Join(err, saveErr)
	}
	encoded, _ := json.Marshal(result)
	return string(encoded), err
}

func (e *Execution) evidence(tree executionTree) *codex.EvidenceBroker {
	documents := map[string]string{}
	for key, value := range e.options.Documents {
		documents[key] = value
	}
	for key, value := range e.options.Skill.Documents {
		documents[key] = value
	}
	encode := func(key string, value any) { data, _ := json.Marshal(value); documents[key] = string(data) }
	documents["original-diff"] = e.options.Snapshot.Diff
	documents["original-commits"] = e.options.Snapshot.Commits
	encode("original-source-gaps", e.options.Snapshot.Gaps)
	for revision, manifest := range e.options.Snapshot.Manifests {
		encode("original-manifest-"+revision, manifest)
	}
	encode("workspace-manifest", tree.Entries)
	encode("workspace-git-metadata", tree.GitMetadata)
	encode("workspace-git-state", tree.Git)
	encode("execution-environment", e.options.Environment)
	encode("workspace-gaps", tree.Gaps)
	encode("native-role-reports", e.journal.Roles)
	encode("command-observations", e.journal.Commands)
	encode("delivery-facts", e.journal.Delivery)
	if e.journal.Delivery.Started {
		documents["delivery-source-boundary"] = "Source/head/workspace evidence is the immutable reviewed candidate before delivery, not a fresh inspection of retained storage. Actual committed/pushed/current PR identity and uncertainties are recorded separately in delivery-facts. Never certify a changed or unobserved workspace from that candidate."
	}
	sources := map[string]map[string]string{}
	for revision, files := range e.options.Snapshot.Sources {
		sources[revision] = files
	}
	sources["head"] = tree.Sources
	return codex.NewEvidenceBroker(sources, documents)
}

func (e *Execution) delegate(ctx context.Context, args json.RawMessage) (string, error) {
	var request struct {
		Role string `json:"role"`
	}
	if err := decodeExecution(args, &request); err != nil {
		return "", err
	}
	if e.journal.Delivery.Started && request.Role != "delivery_auditor" {
		return "", errors.New("the delivery candidate is frozen; only its independent read-only delivery audit remains available")
	}
	skillName := "independent-engineering-workflow"
	switch request.Role {
	case "change_mapper", "billing_invariants", "laravel_boundaries", "go_compatibility":
	case "qa_test_planner":
		skillName = "qa-test-planner"
		for _, report := range e.journal.Roles {
			if report.Role == request.Role {
				return "", errors.New("the bounded QA planning pass already ran; reuse its native report")
			}
		}
		if !e.approved("change_mapper", false) {
			return "", errors.New("map the observable contract before the one QA planning pass")
		}
	case "regression_reviewer":
		if !e.verified() {
			return "", errors.New("regression audit requires current-tree executed verification")
		}
	case "correctness_reviewer":
		skillName = "review"
		if !e.approved("regression_reviewer", true) || !e.verified() {
			return "", errors.New("complete final-diff review requires current-tree regression audit and verification")
		}
	case "delivery_auditor":
		if !e.approved("correctness_reviewer", true) || !e.verified() {
			return "", errors.New("delivery audit requires current-tree final-diff review and verification")
		}
	default:
		return "", errors.New("independent feedback role is not granted")
	}
	tree, err := e.capture(ctx)
	if err != nil {
		return "", err
	}
	// capture may discover an external/native child change, invalidating gates
	// checked above. Recheck at the actual launch boundary.
	if (request.Role == "correctness_reviewer" && !e.approved("regression_reviewer", true)) || (request.Role == "delivery_auditor" && !e.approved("correctness_reviewer", true)) || ((request.Role == "regression_reviewer" || request.Role == "correctness_reviewer" || request.Role == "delivery_auditor") && !e.verified()) {
		return "", errors.New("current source changed; execute affected verification and review again")
	}
	path, err := e.options.Skill.InstructionPath(skillName)
	if err != nil && request.Role == "correctness_reviewer" {
		// Preserve installed legacy workflow bundles without manufacturing an
		// instruction package or silently replacing a captured current review.
		skillName = "requesting-code-review"
		path, err = e.options.Skill.InstructionPath(skillName)
	}
	if err != nil {
		return "", err
	}
	broker := e.evidence(tree)
	prompt := fmt.Sprintf("You are the fresh independent %s, private/read-only/do-not-post; the parent waits for this native report. Audit the authoritative existing-PR requirements, repository instructions, complete original base-to-head change and current workspace tree %s (generation %d). No implementation correctness claim or proposed code structure is supplied. Source revisions base/target are immutable original evidence; head is the complete current candidate text projection, with binary/symlink/missing evidence explicitly identified in workspace-manifest/workspace-gaps. Read the full causal slice and named original-diff/original-commits and target instructions; do not certify unseen gaps. For planning do one bounded pass of concrete risks/assertions/smallest checks; only contradiction, unsafe action or reproducible design defect may block. Do not build an approval program or ask for plan amendments. For regression inspect actual behavior and command observations, not the completeness of the plan. For correctness review the complete final change. For delivery inspect original native role reports, their exact trees and actual delivery facts; never equate local changes with committed/pushed/green CI. Use only read-only evidence tools, no commands or delegation. Return the supplied structured report, keeping native evidence and meaningful findings; APPROVE means conclusive with no unresolved material finding or evidence gap. COMMENT means inconclusive, REQUEST_CHANGES means material defect/gap. Evidence document names: %s.", request.Role, tree.ID, e.journal.Generation, strings.Join(e.documentKeys(tree), ", "))
	if planningRole(request.Role) {
		prompt = fmt.Sprintf("You are the fresh independent preimplementation %s, private/read-only/do-not-post; the parent waits for this bounded INPUT, not approval of implemented code. Read the actual captured selected skill, authoritative requirements/repository instructions, complete original comparison and current source at tree %s, generation %d. No implementation claim or code structure is supplied. Identify concrete affected contracts, consumers, variants, risks and smallest sufficient observable assertions/checks. The original unfixed defect and checks/reviews/delivery that belong AFTER the fix are expected implementation work: record them in findings, NOT blockers. Return exactly summary, findings, blockers under the supplied schema; no review disposition. Blockers may contain only contradictory requirements, an unsafe authorized action or a reproducible invalid design, each with a concrete reason. One bounded QA pass, no amendments or approval program. Do not omit findings to manufacture success, certify an unimplemented fix, or treat completion of this input as current verification/review. Use only read-only evidence tools. Evidence names: %s.", request.Role, tree.ID, e.journal.Generation, strings.Join(e.documentKeys(tree), ", "))
	}
	report := NativeRoleReport{Role: request.Role, Tree: tree.ID, Generation: e.journal.Generation, AfterDelivery: e.journal.Delivery.Started}
	output, runErr := e.options.Agent(ctx, codex.FeedbackRequest{Role: request.Role, Model: e.options.Policy.String("feedback.model"), Effort: e.options.Policy.String("feedback.reasoning_effort"), SkillName: skillName, SkillPath: path, Prompt: prompt, Participant: "specialist", OnInput: e.options.OnInput, OnInputEvent: e.options.OnInputEvent, Tools: feedbackEvidenceTools(), Handle: feedbackEvidenceHandle(broker), OutputSchema: roleOutputSchema(request.Role)})
	report.ThreadID, report.TurnID, report.RequestedModel, report.EffectiveModel, report.Text, report.Usage = output.ThreadID, output.TurnID, output.RequestedModel, output.EffectiveModel, output.Text, output.Usage
	if runErr == nil && (output.ThreadID == "" || output.TurnID == "" || output.ThreadID == e.primaryThread || output.RequestedModel != e.options.Policy.String("feedback.model") || output.EffectiveModel != output.RequestedModel || len(output.Text) > 256<<10) {
		runErr = errors.New("independent role omitted native identity, reused its parent or changed model")
	}
	for _, prior := range e.journal.Roles {
		if prior.ThreadID != "" && prior.ThreadID == output.ThreadID {
			runErr = errors.Join(runErr, errors.New("independent role reused a prior principal/session"))
		}
	}
	var body struct {
		Disposition string   `json:"disposition"`
		Summary     string   `json:"summary"`
		Findings    []string `json:"findings"`
	}
	if runErr == nil && planningRole(request.Role) {
		var input struct {
			Summary  string            `json:"summary"`
			Findings []string          `json:"findings"`
			Blockers []PlanningBlocker `json:"blockers"`
		}
		if decodeExecution(json.RawMessage(output.Text), &input) != nil || strings.TrimSpace(input.Summary) == "" || input.Findings == nil || input.Blockers == nil {
			runErr = errors.New("independent planning role returned an invalid bounded input")
		} else {
			report.Disposition = "PLANNING_COMPLETE"
			for _, blocker := range input.Blockers {
				if (blocker.Kind != "contradictory_requirement" && blocker.Kind != "unsafe_action" && blocker.Kind != "invalid_design") || strings.TrimSpace(blocker.Reason) == "" {
					runErr = errors.New("planning blockers require a concrete invalidating condition")
					break
				}
				report.Disposition = "PLANNING_BLOCKED"
			}
			report.PlanningBlockers = input.Blockers
		}
	} else if runErr == nil {
		if decodeExecution(json.RawMessage(output.Text), &body) != nil || strings.TrimSpace(body.Summary) == "" || body.Findings == nil || (body.Disposition != "APPROVE" && body.Disposition != "COMMENT" && body.Disposition != "REQUEST_CHANGES") {
			runErr = errors.New("independent role returned an invalid or contradictory native disposition")
		} else {
			report.Disposition = body.Disposition
		}
	}
	if runErr != nil {
		report.Error = runErr.Error()
	}
	e.journal.Roles = append(e.journal.Roles, report)
	if err := e.persist(); err != nil {
		return "", errors.Join(runErr, err)
	}
	encoded, _ := json.Marshal(report)
	return string(encoded), runErr
}

func (e *Execution) documentKeys(tree executionTree) []string {
	keys := []string{"original-source-gaps", "original-diff", "original-commits", "workspace-manifest", "workspace-git-metadata", "workspace-git-state", "workspace-gaps", "execution-environment", "native-role-reports", "command-observations", "delivery-facts"}
	if e.journal.Delivery.Started {
		keys = append(keys, "delivery-source-boundary")
	}
	for revision := range e.options.Snapshot.Manifests {
		keys = append(keys, "original-manifest-"+revision)
	}
	for key := range e.options.Documents {
		keys = append(keys, key)
	}
	for key := range e.options.Skill.Documents {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
func roleOutputSchema(role string) map[string]any {
	if planningRole(role) {
		return map[string]any{"type": "object", "additionalProperties": false, "required": []string{"summary", "findings", "blockers"}, "properties": map[string]any{
			"summary": map[string]any{"type": "string"}, "findings": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"blockers": map[string]any{"type": "array", "items": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"kind", "reason"}, "properties": map[string]any{"kind": map[string]any{"type": "string", "enum": []string{"contradictory_requirement", "unsafe_action", "invalid_design"}}, "reason": map[string]any{"type": "string"}}}},
		}}
	}
	return map[string]any{"type": "object", "additionalProperties": false, "required": []string{"disposition", "summary", "findings"}, "properties": map[string]any{"disposition": map[string]any{"type": "string", "enum": []string{"APPROVE", "COMMENT", "REQUEST_CHANGES"}}, "summary": map[string]any{"type": "string"}, "findings": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}}
}

func (e *Execution) disposition(args json.RawMessage) (string, error) {
	if e.journal.Delivery.Started {
		return "", errors.New("delivery froze the original per-item decisions")
	}
	var request struct {
		Key      string `json:"key"`
		Revision string `json:"revision"`
		Status   string `json:"status"`
		Evidence string `json:"evidence"`
	}
	if err := decodeExecution(args, &request); err != nil {
		return "", err
	}
	if strings.TrimSpace(request.Evidence) == "" || len(request.Evidence) > 32768 {
		return "", errors.New("feedback disposition requires a bounded source-backed reason")
	}
	switch request.Status {
	case "accepted", "already_addressed", "rejected", "deferred", "needs_clarification":
	default:
		return "", errors.New("feedback disposition is not a supported per-item decision")
	}
	var selected *Item
	for index := range e.options.Inventory.Items {
		item := &e.options.Inventory.Items[index]
		if item.Key == request.Key && item.Revision == request.Revision {
			selected = item
			break
		}
	}
	if selected == nil {
		return "", errors.New("feedback disposition does not name an exact captured human item/revision")
	}
	disposition := ItemDisposition{Key: selected.Key, Revision: selected.Revision, ThreadID: selected.ThreadID, Status: request.Status, Evidence: request.Evidence}
	found := false
	for index, old := range e.journal.Dispositions {
		if old.Key == selected.Key {
			e.journal.Dispositions[index] = disposition
			found = true
			break
		}
	}
	if !found {
		e.journal.Dispositions = append(e.journal.Dispositions, disposition)
	}
	sort.Slice(e.journal.Dispositions, func(i, j int) bool { return e.journal.Dispositions[i].Key < e.journal.Dispositions[j].Key })
	if err := e.persist(); err != nil {
		return "", err
	}
	data, _ := json.Marshal(disposition)
	return string(data), nil
}

func (e *Execution) deliver(ctx context.Context, args json.RawMessage) (string, error) {
	if e.options.Deliver == nil {
		return "", errors.New("scoped commit/push/GitHub delivery is unavailable")
	}
	if e.journal.Delivery.Started {
		return "", errors.New("delivery was already attempted; inspect canonical facts instead of replaying")
	}
	var proposal DeliveryProposal
	if err := decodeExecution(args, &proposal); err != nil {
		return "", err
	}
	tree, err := e.capture(ctx)
	if err != nil {
		return "", err
	}
	if !e.deliveryReady() {
		return "", errors.New("delivery requires unchanged-candidate checks and independent regression/correctness/delivery reports")
	}
	var retained *retainedCandidate
	if e.options.OwnerRoot != "" {
		retained, err = saveRetainedCandidate(e.options.OwnerRoot, tree)
		if err != nil {
			return "", err
		}
		defer func() {
			if !e.journal.Delivery.Started {
				_ = os.Remove(filepath.Join(e.options.OwnerRoot, retained.Name))
			}
		}()
	}
	record := func(journal DeliveryJournal) error {
		journal.CandidateFile = retained
		e.journal.Delivery = journal
		return e.persist()
	}
	journal, err := e.options.Deliver(ctx, proposal, tree.ID, append([]ItemDisposition{}, e.journal.Dispositions...), record)
	data, _ := json.Marshal(journal)
	return string(data), err
}
