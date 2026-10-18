// Package clarification defines bounded textual data, never tool authority.
package clarification

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"unicode"
	"unicode/utf8"
)

const MaxPayload = 65536
const MaxText = 8192

type Option struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}
type Question struct {
	ID       string   `json:"id"`
	Header   string   `json:"header"`
	Question string   `json:"question"`
	IsOther  bool     `json:"isOther,omitempty"`
	IsSecret bool     `json:"isSecret,omitempty"`
	Options  []Option `json:"options,omitempty"`
}
type Prompt struct {
	Questions []Question `json:"questions"`
}
type Answer struct {
	Answers []string `json:"answers"`
}
type Answers struct {
	Answers map[string]Answer `json:"answers"`
}
type Request struct {
	ID          string     `json:"id"`
	ThreadID    string     `json:"thread_id"`
	TurnID      string     `json:"turn_id"`
	WireID      string     `json:"wire_id"`
	ItemID      string     `json:"item_id"`
	Participant string     `json:"participant"`
	Transport   string     `json:"transport"`
	Questions   []Question `json:"questions"`
}

func Decode(data []byte, into any) error {
	if len(data) > MaxPayload || !utf8.Valid(data) {
		return errors.New("clarification JSON exceeds 64 KiB or is not UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(into) != nil {
		return errors.New("invalid clarification JSON")
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("clarification must contain exactly one JSON object")
	}
	// Duplicate object keys are not consent: encoding/json otherwise silently
	// selects the last occurrence. Check tokens independently at every depth.
	tokens := json.NewDecoder(bytes.NewReader(data))
	if err := uniqueValue(tokens); err != nil {
		return err
	}
	var value any
	if json.Unmarshal(data, &value) != nil {
		return errors.New("invalid clarification JSON")
	}
	return exactSchemaKeys(value, reflect.TypeOf(into))
}

// encoding/json accepts case-insensitive aliases for struct fields, even with
// DisallowUnknownFields. Consent data instead uses exact JSON-tag spellings.
// Map keys (notably question IDs) remain case-sensitive and are not folded.
func exactSchemaKeys(value any, typ reflect.Type) error {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if value == nil || typ == reflect.TypeOf(json.RawMessage{}) {
		return nil
	}
	switch typ.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return nil
		} // Type validity is checked by the decoder.
		fields := map[string]reflect.Type{}
		for i := range typ.NumField() {
			field := typ.Field(i)
			if field.PkgPath != "" {
				continue
			}
			name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			if name == "-" {
				continue
			}
			if name == "" {
				name = field.Name
			}
			fields[name] = field.Type
		}
		for key, child := range object {
			field, ok := fields[key]
			if !ok {
				return errors.New("clarification JSON keys must match exact documented field names")
			}
			if err := exactSchemaKeys(child, field); err != nil {
				return err
			}
		}
	case reflect.Map:
		if object, ok := value.(map[string]any); ok {
			for _, child := range object {
				if err := exactSchemaKeys(child, typ.Elem()); err != nil {
					return err
				}
			}
		}
	case reflect.Slice, reflect.Array:
		if items, ok := value.([]any); ok {
			for _, child := range items {
				if err := exactSchemaKeys(child, typ.Elem()); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func uniqueValue(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return errors.New("invalid clarification JSON")
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		keys := map[string]bool{}
		for d.More() {
			k, err := d.Token()
			if err != nil {
				return errors.New("invalid clarification JSON")
			}
			key, ok := k.(string)
			if !ok || keys[key] {
				return errors.New("duplicate clarification JSON key")
			}
			keys[key] = true
			if err := uniqueValue(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := uniqueValue(d); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid clarification JSON")
	}
	_, err = d.Token()
	return err
}
func Text(value string, maximum int) bool {
	return utf8.ValidString(value) && len(value) <= maximum && strings.TrimSpace(value) != ""
}
func Identity(value string) bool {
	if !Text(value, 512) {
		return false
	}
	for _, c := range value {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}
func ValidateQuestions(questions []Question) error {
	if len(questions) < 1 || len(questions) > 3 {
		return errors.New("clarification requires one to three questions")
	}
	seen := map[string]bool{}
	for _, q := range questions {
		if !Identity(q.ID) || seen[q.ID] || !Text(q.Header, 128) || !Text(q.Question, MaxText) || len(q.Options) > 8 {
			return errors.New("invalid question identity, text or option count")
		}
		seen[q.ID] = true
		labels := map[string]bool{}
		for _, o := range q.Options {
			if !Text(o.Label, 256) || !Text(o.Description, 2048) || labels[o.Label] {
				return errors.New("invalid or duplicate clarification option")
			}
			labels[o.Label] = true
		}
	}
	raw, _ := json.Marshal(Prompt{questions})
	if len(raw) > MaxPayload {
		return errors.New("questions exceed 64 KiB")
	}
	return nil
}
func ValidateRequest(r Request) error {
	if !Identity(r.ID) || !Identity(r.ThreadID) || !Identity(r.TurnID) || !Identity(r.WireID) || !Identity(r.ItemID) || (r.Participant != "primary" && r.Participant != "specialist") || (r.Transport != "native" && r.Transport != "dynamic") {
		return errors.New("clarification request has incomplete session identity")
	}
	return ValidateQuestions(r.Questions)
}
func ValidateAnswers(questions []Question, answers Answers) error {
	if err := ValidateQuestions(questions); err != nil {
		return err
	}
	if len(answers.Answers) != len(questions) {
		return errors.New("answer every question exactly once, without extra question IDs")
	}
	for _, q := range questions {
		a, ok := answers.Answers[q.ID]
		if !ok || len(a.Answers) != 1 || !Text(a.Answers[0], MaxText) {
			return errors.New("each question requires exactly one nonblank UTF-8 answer of at most 8192 bytes")
		}
	}
	raw, _ := json.Marshal(answers)
	if len(raw) > MaxPayload {
		return errors.New("answers exceed 64 KiB")
	}
	return nil
}
func NewID() (string, error) {
	var b [16]byte
	_, err := rand.Read(b[:])
	return hex.EncodeToString(b[:]), err
}
