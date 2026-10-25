package rpc

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"kintsugi/internal/agent"
	"kintsugi/internal/config"
	"kintsugi/internal/harness"
	"kintsugi/internal/provider"
	"kintsugi/internal/session"
	"kintsugi/internal/tools"
)

// SessionManager maps RPC methods to App operations and tracks active sessions.
type SessionManager struct {
	app      *harness.App
	mu       sync.Mutex
	sessions map[string]*activeSession
}

type activeSession struct {
	id     string
	cancel context.CancelFunc
	done   chan struct{}
	result string
	err    error
}

// NewSessionManager creates a session manager backed by the given App.
func NewSessionManager(app *harness.App) *SessionManager {
	return &SessionManager{
		app:      app,
		sessions: make(map[string]*activeSession),
	}
}

// RegisterHandlers registers all RPC methods on the server.
func (m *SessionManager) RegisterHandlers(srv *Server) {
	srv.Register("initialize", m.handleInitialize)
	srv.Register("session/new", m.handleSessionNew)
	srv.Register("session/prompt", m.handleSessionPrompt)
	srv.Register("session/cancel", m.handleSessionCancel)
	srv.Register("session/list", m.handleSessionList)
}

// initializeParams are the parameters for the initialize method.
type initializeParams struct {
	ClientName    string `json:"client_name,omitempty"`
	ClientVersion string `json:"client_version,omitempty"`
}

// initializeResult is the result of the initialize method.
type initializeResult struct {
	ProtocolVersion string          `json:"protocol_version"`
	ServerName      string          `json:"server_name"`
	ServerVersion   string          `json:"server_version"`
	Capabilities    map[string]bool `json:"capabilities"`
}

func (m *SessionManager) handleInitialize(_ context.Context, params json.RawMessage) (any, error) {
	var p initializeParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "invalid params: " + err.Error()}
		}
	}
	return initializeResult{
		ProtocolVersion: "1.0",
		ServerName:      "kintsugi",
		ServerVersion:   "0.8.0-dev",
		Capabilities: map[string]bool{
			"sessions": true,
			"cancel":   true,
		},
	}, nil
}

// sessionNewParams are the parameters for session/new.
type sessionNewParams struct {
	Model   string `json:"model,omitempty"`
	Persona string `json:"persona,omitempty"`
	Role    string `json:"role,omitempty"`
}

// sessionNewResult is the result of session/new.
type sessionNewResult struct {
	SessionID string `json:"session_id"`
	Model     string `json:"model,omitempty"`
	Persona   string `json:"persona,omitempty"`
}

func (m *SessionManager) handleSessionNew(_ context.Context, params json.RawMessage) (any, error) {
	var p sessionNewParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "invalid params: " + err.Error()}
		}
	}

	log, err := session.New()
	if err != nil {
		return nil, &RPCError{Code: CodeInternalError, Message: "session: " + err.Error()}
	}

	model := ""
	persona := ""

	// Resolve model if requested.
	if p.Model != "" || p.Role != "" {
		var res *harness.Resolved
		if p.Role != "" {
			res, err = m.app.ResolveRole(p.Role)
		} else {
			res, err = m.app.Resolve(p.Model)
		}
		if err != nil {
			log.Close()
			return nil, &RPCError{Code: CodeInvalidParams, Message: "resolve: " + err.Error()}
		}
		model = res.ProviderName + "/" + res.Model.ID

		comp, cerr := m.app.CompileRole(res, p.Role, harness.CompileOpts{Persona: p.Persona})
		if cerr != nil {
			log.Close()
			return nil, &RPCError{Code: CodeInternalError, Message: "compile: " + cerr.Error()}
		}
		persona = comp.Manifest.Persona.Name
		log.Log("meta", map[string]string{
			"model":   model,
			"persona": persona,
		})
	}
	log.Close()

	return sessionNewResult{
		SessionID: log.ID,
		Model:     model,
		Persona:   persona,
	}, nil
}

// sessionPromptParams are the parameters for session/prompt.
type sessionPromptParams struct {
	SessionID string `json:"session_id"`
	Prompt    string `json:"prompt"`
	Model     string `json:"model,omitempty"`
	Persona   string `json:"persona,omitempty"`
}

// sessionPromptResult is the result of session/prompt.
type sessionPromptResult struct {
	SessionID string `json:"session_id"`
	Text      string `json:"text"`
}

func (m *SessionManager) handleSessionPrompt(ctx context.Context, params json.RawMessage) (any, error) {
	var p sessionPromptParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &RPCError{Code: CodeInvalidParams, Message: "invalid params: " + err.Error()}
	}
	if p.SessionID == "" {
		return nil, &RPCError{Code: CodeInvalidParams, Message: "session_id is required"}
	}
	if p.Prompt == "" {
		return nil, &RPCError{Code: CodeInvalidParams, Message: "prompt is required"}
	}

	// Resolve model.
	res, err := m.app.Resolve(p.Model)
	if err != nil {
		return nil, &RPCError{Code: CodeInvalidParams, Message: "resolve: " + err.Error()}
	}
	if res.Client.APIKey == "" && !config.IsLoopback(res.Client.BaseURL) {
		return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("provider %q needs %s", res.ProviderName, res.ProviderCfg.APIKeyEnv)}
	}

	comp, err := m.app.Compile(res, harness.CompileOpts{Persona: p.Persona})
	if err != nil {
		return nil, &RPCError{Code: CodeInternalError, Message: "compile: " + err.Error()}
	}

	// Open or create session.
	var log *session.Session
	var history []provider.Message
	log, err = session.Open(p.SessionID)
	if err != nil {
		// If session doesn't exist, create it.
		log, err = session.New()
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: "session: " + err.Error()}
		}
		if log.ID != p.SessionID {
			// The new session got a different ID. That's fine, we use the new one.
		}
		log.Log("meta", map[string]string{
			"model":   res.ProviderName + "/" + res.Model.ID,
			"persona": comp.Manifest.Persona.Name,
		})
	} else {
		history, _ = session.History(p.SessionID)
	}
	defer log.Close()
	m.app.SessionID = log.ID

	// Set up tools.
	mem, _ := m.app.MemoryStore()
	if mem != nil {
		defer mem.Close()
	}
	policy := m.app.ToolPolicy()
	ex := &tools.Executor{
		Workspace: m.app.Workspace,
		Memory:    mem,
		Policy:    policy,
		Log:       log.Log,
		Offline:   m.app.Cfg.Core.Mode == "offline",
	}
	ex.ApplySecurity(m.app.Cfg)
	closeTools := m.app.AttachTools(ex, log.Log)
	defer closeTools()

	// Build the agent loop.
	var finalText string
	loop := &agent.Loop{
		Client:   res.Client,
		Tools:    ex,
		Log:      log,
		MaxLoops: m.app.Cfg.Harness.MaxToolLoops,
		OnDelta:  func(s string) { finalText += s },
		OnUsage:  func(p, c int) { m.app.RecordUsage(res.ProviderName, p, c) },
	}
	loop.CompactAfter = m.app.Cfg.Harness.CompactAfter
	loop.CompactKeepRatio = m.app.Cfg.Harness.CompactKeepRatio
	loop.Summarize = m.app.Summarizer()

	// Create a cancellable context for this prompt.
	promptCtx, cancel := context.WithCancel(ctx)
	active := &activeSession{
		id:     log.ID,
		cancel: cancel,
		done:   make(chan struct{}),
	}

	m.mu.Lock()
	m.sessions[log.ID] = active
	m.mu.Unlock()

	defer func() {
		cancel()
		m.mu.Lock()
		delete(m.sessions, log.ID)
		m.mu.Unlock()
	}()

	// Run the conversation.
	cl := &agent.CompactLoop{
		Loop:             loop,
		CompactAfter:     m.app.Cfg.Harness.CompactAfter,
		CompactKeepRatio: m.app.Cfg.Harness.CompactKeepRatio,
		Summarize:        agent.NewSummarizer(res.Client),
	}
	result, _, err := cl.RunConversation(promptCtx, comp.Text, history, p.Prompt)
	if err != nil {
		if promptCtx.Err() != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: "cancelled"}
		}
		return nil, &RPCError{Code: CodeInternalError, Message: err.Error()}
	}

	log.Log("user", p.Prompt)
	log.Log("assistant", result)

	return sessionPromptResult{
		SessionID: log.ID,
		Text:      result,
	}, nil
}

func (m *SessionManager) handleSessionCancel(_ context.Context, params json.RawMessage) (any, error) {
	var p struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &RPCError{Code: CodeInvalidParams, Message: "invalid params: " + err.Error()}
	}
	if p.SessionID == "" {
		return nil, &RPCError{Code: CodeInvalidParams, Message: "session_id is required"}
	}

	m.mu.Lock()
	active, ok := m.sessions[p.SessionID]
	m.mu.Unlock()

	if !ok {
		return nil, &RPCError{Code: CodeInvalidParams, Message: "session not found or not active"}
	}
	active.cancel()
	return map[string]bool{"cancelled": true}, nil
}

// sessionListResult is one entry in the session/list response.
type sessionListEntry struct {
	ID      string `json:"id"`
	Title   string `json:"title,omitempty"`
	Model   string `json:"model,omitempty"`
	Persona string `json:"persona,omitempty"`
	Turns   int    `json:"turns"`
	Started string `json:"started,omitempty"`
}

func (m *SessionManager) handleSessionList(_ context.Context, _ json.RawMessage) (any, error) {
	infos, err := session.ListInfo()
	if err != nil {
		return nil, &RPCError{Code: CodeInternalError, Message: "list: " + err.Error()}
	}
	entries := make([]sessionListEntry, 0, len(infos))
	for _, info := range infos {
		e := sessionListEntry{
			ID:      info.ID,
			Title:   info.DisplayTitle(),
			Model:   info.Model,
			Persona: info.Persona,
			Turns:   info.Turns,
		}
		if !info.Started.IsZero() {
			e.Started = info.Started.Format("2006-01-02T15:04:05Z")
		}
		entries = append(entries, e)
	}
	return map[string]any{"sessions": entries}, nil
}
