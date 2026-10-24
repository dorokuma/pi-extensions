package jevgate

// Question tables of jev-gate (TypeScript authority:
// extensions/jev-gate/src/jev-gate.ts, the CONTRACT_FIELDS / GATE_QUESTIONS /
// TRIAGE_CRITERIA / SOURCE_QUESTIONS / DISAGREEMENT_QUESTIONS /
// DISAGREEMENT_RELATION tables).
//
// The tables are inline here for the same reason the TS file inlines them: the
// deployed artefact is a single module, and the calibration script (TS side)
// extracts the tables from the source file. Keys are the question ids the
// classifier answers; the instructions stay English (Jev/Clef is trained mainly
// on English), while the state (payload) may be Chinese.
//
// Ordering is part of the contract: verdict lines, the "probs:" line and the
// request body all follow table order, exactly as Object.entries does in TS.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Task is one of the five fixed jev_check tasks (TS: TASKS).
type Task string

// The five tasks (TS: `const TASKS = [...] as const`).
const (
	TaskContractLint Task = "contract_lint"
	TaskGateScan     Task = "gate_scan"
	TaskReturnTriage Task = "return_triage"
	TaskDisagreement Task = "disagreement"
	TaskSourceAudit  Task = "source_audit"
)

// Tasks lists the tasks in TS order (the error message and the schema enum use it).
var Tasks = []Task{TaskContractLint, TaskGateScan, TaskReturnTriage, TaskDisagreement, TaskSourceAudit}

// Role is one of the dispatch roles (TS: ROLES). Only contract_lint consumes it.
type Role string

// The seven roles (TS: `const ROLES = [...] as const`).
const (
	RoleScout      Role = "scout"
	RolePlanner    Role = "planner"
	RoleResearcher Role = "researcher"
	RoleWorker     Role = "worker"
	RoleVerifier   Role = "verifier"
	RoleReviewer   Role = "reviewer"
	RoleOracle     Role = "oracle"
)

// Roles lists the roles in TS order.
var Roles = []Role{RoleScout, RolePlanner, RoleResearcher, RoleWorker, RoleVerifier, RoleReviewer, RoleOracle}

// taskList renders TASKS.join("|").
func taskList() string {
	names := make([]string, len(Tasks))
	for i, task := range Tasks {
		names[i] = string(task)
	}
	return strings.Join(names, "|")
}

// roleList renders ROLES.join("|").
func roleList() string {
	names := make([]string, len(Roles))
	for i, role := range Roles {
		names[i] = string(role)
	}
	return strings.Join(names, "|")
}

// Criterion is one choice label with the sentence describing when it applies
// (the TypeScript `criteria` record entry).
type Criterion struct {
	Label       string
	Description string
}

// Question is one classifier question: either `{type:"noul", instructions}` or
// `{type:"choice", instructions, criteria}` (TS: noul() / choice()).
type Question struct {
	ID           string
	Type         string
	Instructions string
	Criteria     []Criterion
}

// noul builds a "is the proposition true" question; the classifier answers with
// a probability (TS: noul()).
func noul(id, instructions string) Question {
	return Question{ID: id, Type: "noul", Instructions: instructions}
}

// choice builds a labelled-choice question (TS: choice()).
func choice(id, instructions string, criteria []Criterion) Question {
	return Question{ID: id, Type: "choice", Instructions: instructions, Criteria: criteria}
}

// MarshalJSON writes the question in the wire shape the System One endpoint
// expects: noul questions carry type+instructions, choice questions add the
// ordered criteria object.
func (q Question) MarshalJSON() ([]byte, error) {
	var out bytes.Buffer
	out.WriteByte('{')
	writeJSONMember(&out, "type", q.Type, true)
	writeJSONMember(&out, "instructions", q.Instructions, false)
	if q.Type == "choice" {
		out.WriteString(`,"criteria":{`)
		for i, criterion := range q.Criteria {
			if i > 0 {
				out.WriteByte(',')
			}
			key, err := json.Marshal(criterion.Label)
			if err != nil {
				return nil, err
			}
			value, err := json.Marshal(criterion.Description)
			if err != nil {
				return nil, err
			}
			out.Write(key)
			out.WriteByte(':')
			out.Write(value)
		}
		out.WriteByte('}')
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

// writeJSONMember writes `,"key":value`; the first member omits the comma and
// uses the raw (already-encoded) value.
func writeJSONMember(out *bytes.Buffer, key string, value any, first bool) {
	if !first {
		out.WriteByte(',')
	}
	encodedKey, err := json.Marshal(key)
	if err != nil {
		return
	}
	out.Write(encodedKey)
	out.WriteByte(':')
	encodedValue, err := json.Marshal(value)
	if err != nil {
		return
	}
	out.Write(encodedValue)
}

// Questions is an ordered question map: the request body is `{"id": question}`
// in table order, matching the JavaScript object Object.entries walks.
type Questions []Question

// MarshalJSON writes the questions as one ordered JSON object.
func (qs Questions) MarshalJSON() ([]byte, error) {
	var out bytes.Buffer
	out.WriteByte('{')
	for i, question := range qs {
		if i > 0 {
			out.WriteByte(',')
		}
		key, err := json.Marshal(question.ID)
		if err != nil {
			return nil, err
		}
		value, err := question.MarshalJSON()
		if err != nil {
			return nil, err
		}
		out.Write(key)
		out.WriteByte(':')
		out.Write(value)
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

// Keys lists the question ids in table order (TS: Object.keys(questions)).
func (qs Questions) Keys() []string {
	keys := make([]string, len(qs))
	for i, question := range qs {
		keys[i] = question.ID
	}
	return keys
}

// Get returns the question with id, or false when the table has none.
func (qs Questions) Get(id string) (Question, bool) {
	for _, question := range qs {
		if question.ID == id {
			return question, true
		}
	}
	return Question{}, false
}

// contractFields are the hard fields of each role's output contract (TS:
// CONTRACT_FIELDS). The assertions are verbatim from the TS table.
var contractFields = map[Role][]Question{
	RoleScout: {
		noul("has_locations", "Does the report give concrete file paths or symbol names WITH line numbers?"),
		noul("has_relationships", "Does it describe caller/callee or impact relationships between the pieces?"),
		noul("has_risk_clues", "Does it list observed risk clues (stated as facts, without severity claims)?"),
		noul("has_starting_point", "Does it name which file the downstream role should open first, and why?"),
	},
	RolePlanner: {
		noul("has_goal", "Does the plan state the goal in one sentence?"),
		noul("has_steps_with_targets", "Are the steps numbered and pointed at concrete files, functions or line numbers?"),
		noul("has_changed_files", "Does it list which files will be changed?"),
		noul("has_risks_and_rollback", "Does it list risk points together with a rollback strategy?"),
		noul("has_verify_commands", "Does it give the commands that will verify the change?"),
	},
	RoleResearcher: {
		noul("has_sources", "Does every conclusion carry the source URL it came from?"),
		noul("has_actionable_conclusion", "Does it give an actionable conclusion, rather than only a list of links?"),
		noul("marks_unverified", "Does it mark single-source claims as not cross-verified?"),
	},
	RoleWorker: {
		noul("has_completed_list", "Does the report list what was changed and why, item by item?"),
		noul("has_files_changed", "Does it list the paths of the files it changed?"),
		noul("has_baseline_statement", "Does it state what baseline was run before the change?"),
		noul("shows_verification_commands", "Does it show the exact build/test commands and their actual results?"),
	},
	RoleVerifier: {
		noul("has_environment", "Does the report name the isolated copy path it used and the baseline commit or hash?"),
		noul("has_commands", "Does it list the commands that were executed?"),
		noul("has_evidence_quadruple", "For each criterion, does it give the exit code, raw output, artifact hash and duration?"),
		noul("no_conclusion", "Does it avoid giving any conclusion, severity, recommendation or fix?"),
	},
	RoleReviewer: {
		noul("has_verdict", "Does it state whether the change can be released?"),
		noul("has_findings_with_location", "Is every finding located by file and line?"),
		noul("has_severity_levels", "Are the findings classified into severity levels (fatal / should-fix / suggestion)?"),
		noul("has_concrete_advice", "Are the suggestions concrete and actionable rather than generic style remarks?"),
	},
	RoleOracle: {
		noul("has_conclusion_first", "Does it lead with a confirm-or-refute conclusion together with its basis?"),
		noul("has_tiered_findings", "Are the findings tiered, each with file and line, a trigger condition and a counter-path?"),
		noul("has_counter_questions", "Does it leave the decision-critical questions open for the caller to answer?"),
	},
}

// gateQuestions scan for gate defects; the question direction is "the defect
// exists", so a higher probability means a stronger reason to block (TS:
// GATE_QUESTIONS).
var gateQuestions = Questions{
	noul("g1_evidence_missing_fields", "Does the verification evidence lack any of these: environment, executed commands, exit code, raw output, artifact hash, duration?"),
	noul("g1_criterion_not_executed", "Is any verification criterion classified as not-run (timeout or refusal) or evidence-incomplete, rather than actually executed and passing?"),
	noul("g2_unresolved_must_fix", "Is there any fatal or must-fix item that is NOT marked as fixed and re-verified?"),
	noul("g3_unresolved_fatal_doubt", "Is there any unresolved fatal objection or blind spot raised against this change?"),
	noul("g4_leftovers_unrecorded", "Based only on the follow-up list included in the text, is there any should-fix or observation item mentioned in the conclusions that is absent from that list and was not explicitly waived?"),
}

// triageCriteria are the return-classification labels, isomorphic to the
// existing anomaly list (TS: TRIAGE_CRITERIA).
var triageCriteria = []Criterion{
	{Label: "normal_delivery", Description: "Normal delivery: it states a completion conclusion and contains the required outputs."},
	{Label: "provider_error", Description: "Provider or upstream error: an error status, upstream failure or aborted run."},
	{Label: "empty_return", Description: "Empty or missing conclusion: no substantive body, or the required output is absent."},
	{Label: "no_artifact_idle", Description: "Idle with no artifact: text is present but the task goal was never reached."},
	{Label: "truncated", Description: "Visible truncation: the report breaks off and a required part is missing."},
	{Label: "exec_timeout", Description: "Execution timeout: it says a step could not run within the time limit."},
	{Label: "refusal", Description: "Refusal: the model or provider declined to perform the task."},
	{Label: "evidence_incomplete", Description: "Evidence incomplete: the verifier could not produce complete evidence."},
}

// sourceQuestions audit a researcher report's sourcing.
var sourceQuestions = Questions{
	noul("all_claims_sourced", "Does every conclusion in the text carry a source of its own?"),
	noul("has_verifiable_urls", "Does the text contain at least one http:// or https:// link?"),
	noul("marks_unverified", "Does the text say that a claim came from a single source and was not cross-verified?"),
}

// disagreementQuestions extract reviewer/oracle disagreement (the payload is
// report A, a line of three dashes, then report B).
var disagreementQuestions = Questions{
	noul("has_material_disagreement", "Does the part after the dashes mention a finding that the part before the dashes does not mention, or contradict such a finding?"),
	noul("same_finding_set", "Do the part before the dashes and the part after the dashes discuss the same findings?"),
}

// disagreementRelations are the relation labels of the disagreement task.
var disagreementRelations = []Criterion{
	{Label: "agrees", Description: "B agrees with A on the findings and their severity."},
	{Label: "b_raises_new", Description: "B raises findings or doubts that A did not mention."},
	{Label: "b_refutes_a", Description: "B refutes or contradicts a finding that A stated."},
	{Label: "both_raise_different", Description: "Both sides raise what the other did not mention."},
	{Label: "b_defers", Description: "B defers or leaves the question open instead of judging."},
}

// questionsFor builds the question table of one task (TS: questionsFor).
func questionsFor(task Task, role *Role) (Questions, error) {
	switch task {
	case TaskContractLint:
		if role == nil {
			return nil, fmt.Errorf("contract_lint requires role in %s", roleList())
		}
		fields, ok := contractFields[*role]
		if !ok {
			return nil, fmt.Errorf("contract_lint requires role in %s", roleList())
		}
		return append(Questions(nil), fields...), nil
	case TaskGateScan:
		return append(Questions(nil), gateQuestions...), nil
	case TaskReturnTriage:
		return Questions{
			choice("kind", "Classify this subagent return. Choose the single best label.", triageCriteria),
			noul("states_conclusion", "Does the text state an explicit completion conclusion and contain the required outputs of its role?"),
		}, nil
	case TaskDisagreement:
		questions := append(Questions(nil), disagreementQuestions...)
		questions = append(questions, choice("relation", "Compared with report A, what does report B do about the findings? Answer from the text only.", disagreementRelations))
		return questions, nil
	case TaskSourceAudit:
		return append(Questions(nil), sourceQuestions...), nil
	default:
		return nil, fmt.Errorf("unknown task %s", string(task))
	}
}
