package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
)

type androidRequest struct {
	Op               string          `json:"op"`
	CompleteReadings bool            `json:"completeReadings"`
	Revision         *int            `json:"revision"`
	Base             string          `json:"base"`
	State            json.RawMessage `json:"state"`
	Source           string          `json:"source"`
	Action           string          `json:"action"`
	GoalID           string          `json:"goalId"`
	Request          string          `json:"request"`
}

func (s *Store) androidRequest(raw []byte) (object, error) {
	var r androidRequest
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, ErrInvalid
	}
	switch r.Op {
	case "read":
		if r.CompleteReadings {
			var user string
			err := s.db.QueryRow("SELECT user_id FROM android_dossier WHERE slot=1").Scan(&user)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			if user != "" {
				if err = s.completeStoredReadings(user); err != nil {
					return nil, err
				}
			}
		}
		return s.androidState(nil, nil, "")
	case "write":
		if r.Revision == nil || *r.Revision < 0 {
			return nil, ErrInvalid
		}
		state, err := parseObject(r.State)
		if err != nil {
			return nil, err
		}
		return s.androidState(r.Revision, state, r.Base)
	case "tick":
		return s.runProactiveTick(r.Source)
	case "policy":
		return proactivePolicyReply(s.dbPath), nil
	case "goalreview":
		if r.GoalID == "" || !bounded(r.Request, grMaxRequest) {
			return nil, ErrInvalid
		}
		action := r.Action
		if action == "" {
			action = grActionShow
		}
		review, err := s.GoalReview("", r.GoalID, action, r.Request)
		if err != nil {
			return nil, err
		}
		// The review write advanced the dossier revision. Return the token the
		// WebView must adopt so its next edit is based on the state it just read.
		state, err := s.androidState(nil, nil, "")
		if err != nil {
			return nil, err
		}
		return object{"ok": true, "review": review, "revision": state["revision"], "snapshot": state["snapshot"]}, nil
	default:
		return nil, ErrInvalid
	}
}

func runAndroidBridge(s *Store, in io.Reader, out io.Writer) error {
	reader := bufio.NewReader(in)
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			response, requestErr := s.androidRequest(line)
			if requestErr != nil {
				code := "storage_error"
				if errors.Is(requestErr, ErrConflict) {
					code = "revision_changed"
				} else if errors.Is(requestErr, ErrInvalid) {
					code = "invalid_input"
				} else if errors.Is(requestErr, ErrNotFound) {
					code = "not_found"
				}
				response = object{"ok": false, "error": code}
			}
			if writeErr := enc.Encode(response); writeErr != nil {
				return writeErr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
