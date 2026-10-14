package codex

import (
	"bytes"
	"encoding/json"
	"errors"
)

// Status contains application-selected activity and current execution facts,
// never streamed reasoning, model text, tool arguments or source contents.
func reportStatus(request ReviewRequest, result *ReviewOutput, activity string) error {
	result.Progress = activity
	if request.OnStatus == nil {
		return nil
	}
	snapshot := *result
	snapshot.Text = ""
	snapshot.Usage = append(json.RawMessage(nil), result.Usage...)
	snapshot.Specialists = nil
	return request.OnStatus(snapshot)
}

func recordActiveStatus(request ReviewRequest, result *ReviewOutput, event Message) error {
	var body struct {
		ThreadID, TurnID, ToModel string
		Item                      struct{ Type string }
		Tool                      string
	}
	if json.Unmarshal(event.Params, &body) != nil || body.ThreadID != result.ThreadID || body.TurnID != result.TurnID {
		return nil
	}
	activity := result.Progress
	switch event.Method {
	case "thread/tokenUsage/updated":
		if bytes.Equal(result.Usage, event.Params) {
			return nil
		}
		result.Usage = append(json.RawMessage(nil), event.Params...)
		return reportStatus(request, result, activity)
	case "model/rerouted":
		if body.ToModel == "" {
			return errors.New("Codex reroute omitted its effective model")
		}
		if body.ToModel == result.EffectiveModel {
			return nil
		}
		result.EffectiveModel = body.ToModel
		return reportStatus(request, result, activity)
	case "item/tool/call":
		activity = "reading_evidence"
		if body.Tool == "reviewd_request_input" {
			activity = "waiting_input"
		}
		if body.Tool == "reviewd_delegate_specialist" {
			activity = "delegating"
		}
	case "item/tool/requestUserInput":
		activity = "waiting_input"
	case "item/started":
		switch body.Item.Type {
		case "reasoning":
			activity = "thinking"
		case "agentMessage":
			activity = "responding"
		}
	}
	if activity == result.Progress {
		return nil
	}
	return reportStatus(request, result, activity)
}
