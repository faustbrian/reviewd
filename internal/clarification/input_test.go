package clarification

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTextClarificationValidation(t *testing.T) {
	q := []Question{{ID: "scope", Header: "Scope", Question: "Which variant?"}, {ID: "private", Header: "Private", Question: "Token?", IsSecret: true}}
	a := Answers{map[string]Answer{"scope": {[]string{"Billing"}}, "private": {[]string{"λ🔑"}}}}
	if ValidateAnswers(q, a) != nil {
		t.Fatal("valid Unicode answers rejected")
	}
	for _, raw := range []string{`{"answers":{"scope":{"answers":["a"]},"scope":{"answers":["b"]}}}`, `{"answers":{}} {}`, `{"answers":{},"unknown":true}`, "\xff"} {
		var value Answers
		if Decode([]byte(raw), &value) == nil {
			t.Fatal("malformed or ambiguous answers accepted")
		}
	}
	for _, mutate := range []func([]Question, Answers){
		func(q []Question, a Answers) { q[1].ID = q[0].ID },
		func(q []Question, a Answers) { delete(a.Answers, "scope") },
		func(q []Question, a Answers) { a.Answers["extra"] = Answer{[]string{"x"}} },
		func(q []Question, a Answers) { a.Answers["scope"] = Answer{[]string{" "}} },
		func(q []Question, a Answers) { a.Answers["scope"] = Answer{[]string{"a", "b"}} },
		func(q []Question, a Answers) { a.Answers["scope"] = Answer{[]string{strings.Repeat("a", MaxText+1)}} },
	} {
		copied := append([]Question{}, q...)
		raw, _ := json.Marshal(a)
		var answer Answers
		_ = json.Unmarshal(raw, &answer)
		mutate(copied, answer)
		if ValidateAnswers(copied, answer) == nil {
			t.Fatal("invalid question/answer mapping accepted")
		}
	}
	a.Answers["scope"] = Answer{[]string{strings.Repeat("a", MaxText)}}
	if ValidateAnswers(q, a) != nil {
		t.Fatal("declared exact answer limit rejected")
	}
	if ValidateQuestions(nil) == nil || ValidateQuestions(append(q, q...)) == nil {
		t.Fatal("question count limits not enforced")
	}
}

func TestSchemaFieldCaseAliasesAreRejectedWithoutFoldingQuestionIDs(t *testing.T) {
	for _, raw := range []string{
		`{"answers":{"scope":{"answers":["first"]}},"Answers":{"scope":{"answers":["second"]}}}`,
		`{"answers":{"scope":{"answers":["first"],"Answers":["second"]}}}`,
		`{"Answers":{"scope":{"answers":["second"]}}}`,
	} {
		var answers Answers
		if Decode([]byte(raw), &answers) == nil {
			t.Fatal("case-aliased answer schema silently accepted")
		}
	}
	var prompt Prompt
	if Decode([]byte(`{"questions":[{"id":"first","ID":"second","header":"Scope","question":"Which?"}]}`), &prompt) == nil {
		t.Fatal("case-aliased question identity accepted")
	}
	raw := []byte(`{"answers":{"scope":{"answers":["lower"]},"Scope":{"answers":["upper"]}}}`)
	var answers Answers
	if err := Decode(raw, &answers); err != nil {
		t.Fatal("question ID map keys were incorrectly case-folded")
	}
	if err := ValidateAnswers([]Question{{ID: "scope", Header: "Lower", Question: "Which lower?"}, {ID: "Scope", Header: "Upper", Question: "Which upper?"}}, answers); err != nil {
		t.Fatal(err)
	}
}
