package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var pluginVersion = "0.1.12"

type rpcAuthStart struct {
	pluginapi.AuthLoginStartRequest
	CallbackID string `json:"host_callback_id"`
}
type rpcAuthPoll struct {
	pluginapi.AuthLoginPollRequest
	CallbackID string `json:"host_callback_id"`
}
type rpcAuthRefresh struct {
	pluginapi.AuthRefreshRequest
	CallbackID string `json:"host_callback_id"`
}
type rpcModel struct {
	pluginapi.AuthModelRequest
	CallbackID string `json:"host_callback_id"`
}
type rpcQuota struct {
	pluginapi.QuotaFetchRequest
	CallbackID string `json:"host_callback_id"`
}
type rpcManagement struct {
	pluginapi.ManagementRequest
	CallbackID string `json:"host_callback_id"`
}
type rpcExecution struct {
	pluginapi.ExecutorRequest
	CallbackID string `json:"host_callback_id"`
	StreamID   string `json:"stream_id"`
}

func unmarshal(raw []byte, out any) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if json.Unmarshal(raw, out) != nil {
		return pluginabi.NewError("invalid_request", "invalid plugin request", 400)
	}
	return nil
}

// authEntries publishes one entry per build. The identifiers are also the values
// persisted as oauth_provider, so a console can attribute a credential to its card
// and a re-login can find its build. Each entry carries its build's own mark, so the
// two cards are distinguishable at a glance.
func authEntries() []pluginapi.AuthProviderEntry {
	return []pluginapi.AuthProviderEntry{
		{Identifier: siteWorkBuddy, Name: "WorkBuddy", Logo: resourceAssetURL(workbuddyMark)},
		{Identifier: siteWorkBuddyAI, Name: "WorkBuddy AI", Logo: resourceAssetURL(aiLogo)},
	}
}

func registration() any {
	return map[string]any{
		"schema_version": pluginabi.SchemaVersion,
		"metadata": pluginapi.Metadata{
			Name:             "OMC WorkBuddy OAuth",
			Version:          pluginVersion,
			Author:           "JUNZE",
			GitHubRepository: "https://github.com/omc-community/omc-workbuddy-oauth",
			Logo:             resourceAssetURL(pluginLogo),
			ConfigFields: []pluginapi.ConfigField{
				{Name: "site", Type: pluginapi.ConfigFieldTypeEnum, Default: siteWorkBuddy, EnumValues: []string{siteWorkBuddy, siteWorkBuddyAI}, Description: "Fallback sign-in build for a login request that names no entry: workbuddy (the default, the China build) or workbuddy-ai (the international build). The panel's WorkBuddy / WorkBuddy AI entries pick their own build and do not read this."},
				{Name: "max_concurrency", Type: pluginapi.ConfigFieldTypeInteger, Default: 3, Description: "Maximum simultaneous requests per credential (default 3)."},
				{Name: "app_version", Type: pluginapi.ConfigFieldTypeString, Default: defaultAppVersion, Description: "WorkBuddy desktop version the client claims in its User-Agent (default 5.9.3). WorkBuddy refuses a chat that does not name a WorkBuddy client."},
				{Name: "cli_version", Type: pluginapi.ConfigFieldTypeString, Default: defaultCLIVersion, Description: "WorkBuddy CLI version the client claims in its User-Agent (default 2.165.0)."},
				{Name: "exclude_models", Type: pluginapi.ConfigFieldTypeArray, Default: []string{}, Description: "Model ids this plugin never advertises. WorkBuddy's product config lists models an account's plan does not include, and that is only learned by being refused; naming them here keeps CPA from routing those models to this credential in the first place. A refused model is otherwise hidden on its own for twelve hours."},
			},
		},
		"capabilities": map[string]any{
			"auth_provider":           true,
			"model_provider":          true,
			"executor":                true,
			"executor_model_scope":    "oauth",
			"executor_input_formats":  []string{"openai"},
			"executor_output_formats": []string{"openai"},
			"quota_provider":          true,
			"management_api":          true,
		},
	}
}

func handleMethod(method string, raw []byte) (any, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		var r struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if err := unmarshal(raw, &r); err != nil {
			return nil, err
		}
		if err := configure(r.ConfigYAML); err != nil {
			return nil, pluginabi.NewError("invalid_config", err.Error(), 400)
		}
		return registration(), nil
	case "plugin.quiesce", "plugin.shutdown":
		return map[string]any{}, nil
	case "auth.identifier":
		// entries is schema 10; identifiers alone keep older hosts working.
		// credential_providers tells the host that credentials typed "workbuddy"
		// belong to this plugin.
		return map[string]any{
			"identifier":           credentialProvider,
			"identifiers":          []string{siteWorkBuddy, siteWorkBuddyAI},
			"credential_providers": []string{credentialProvider},
			"entries":              authEntries(),
		}, nil
	case "executor.identifier", "quota.identifier":
		// The executor and quota providers route by the credential key, not by a
		// login entry: both builds share one provider.
		return map[string]string{"identifier": credentialProvider}, nil
	case "auth.parse":
		var r pluginapi.AuthParseRequest
		if err := unmarshal(raw, &r); err != nil {
			return nil, err
		}
		setAuthDir(r.Host.AuthDir)
		return parseAuth(r), nil
	case "auth.login.start":
		var r rpcAuthStart
		if err := unmarshal(raw, &r); err != nil {
			return nil, err
		}
		setAuthDir(r.Host.AuthDir)
		v, err := startLogin(r.AuthLoginStartRequest, cpaSender{callbackID: r.CallbackID})
		if err != nil {
			return nil, pluginabi.NewError("auth_error", err.Error(), 502)
		}
		return v, nil
	case "auth.login.poll":
		var r rpcAuthPoll
		if err := unmarshal(raw, &r); err != nil {
			return nil, err
		}
		setAuthDir(r.Host.AuthDir)
		v, err := pollLogin(r.AuthLoginPollRequest, cpaSender{callbackID: r.CallbackID})
		if err != nil {
			return nil, pluginabi.NewError("auth_error", err.Error(), 400)
		}
		return v, nil
	case "auth.refresh":
		var r rpcAuthRefresh
		if err := unmarshal(raw, &r); err != nil {
			return nil, err
		}
		setAuthDir(r.Host.AuthDir)
		v, err := refreshCredential(r.AuthRefreshRequest, cpaSender{callbackID: r.CallbackID})
		if err != nil {
			var expired *expiredError
			if errors.As(err, &expired) {
				return nil, pluginabi.NewError("reauth_required", err.Error(), 401)
			}
			return nil, pluginabi.NewError("refresh_error", err.Error(), 503)
		}
		return v, nil
	case "management.register":
		return map[string]any{"resources": []map[string]any{
			{"path": "/" + pluginLogo, "menu": "", "description": "Plugin sticker mark: the plugin's face in a store."},
			{"path": "/" + workbuddyMark, "menu": "", "description": "WorkBuddy mark for the China build's sign-in entry."},
			{"path": "/" + aiLogo, "menu": "", "description": "CodeBuddy mark for the WorkBuddy AI sign-in entry."},
			{"path": "/omc.json", "menu": "", "description": "Public, optional OMC plugin declaration."},
		}}, nil
	case "management.handle":
		var r rpcManagement
		if err := unmarshal(raw, &r); err != nil {
			return nil, err
		}
		return handleManagementResource(r.ManagementRequest)
	case "model.static":
		return staticModels(), nil
	case "model.for_auth":
		var r rpcModel
		if err := unmarshal(raw, &r); err != nil {
			return nil, err
		}
		setAuthDir(r.Host.AuthDir)
		v, err := modelsForAuth(r.AuthModelRequest, cpaSender{callbackID: r.CallbackID})
		if err != nil {
			return nil, pluginabi.NewError("model_error", err.Error(), 502)
		}
		return v, nil
	case "quota.describe":
		return quotaDescription(), nil
	case "quota.fetch":
		var r rpcQuota
		if err := unmarshal(raw, &r); err != nil {
			return nil, err
		}
		setAuthDir(r.Host.AuthDir)
		return quotaFetch(r.QuotaFetchRequest, cpaSender{callbackID: r.CallbackID})
	case "quota.reset":
		return quotaReset(), nil
	case "executor.execute":
		var r rpcExecution
		if err := unmarshal(raw, &r); err != nil {
			return nil, err
		}
		return execute(r.ExecutorRequest, cpaSender{callbackID: r.CallbackID})
	case "executor.execute_stream":
		var r rpcExecution
		if err := unmarshal(raw, &r); err != nil {
			return nil, err
		}
		return executeStream(r.ExecutorRequest, r.CallbackID, r.StreamID, cpaSender{callbackID: r.CallbackID})
	case "executor.count_tokens":
		return countTokens()
	case "executor.http_request":
		return nil, pluginabi.NewError("not_supported", "unsupported executor HTTP request", http.StatusNotImplemented)
	default:
		return nil, pluginabi.NewError("unknown_method", "unknown plugin method", http.StatusNotImplemented)
	}
}

// setAuthDir records where CPA keeps auth material, for the state key and the
// pending sign-in flows. An empty value never overwrites a known directory.
var authDirState struct {
	sync.RWMutex
	dir string
}

func setAuthDir(dir string) {
	if dir == "" {
		return
	}
	authDirState.Lock()
	authDirState.dir = dir
	authDirState.Unlock()
}

func knownAuthDir() string {
	authDirState.RLock()
	defer authDirState.RUnlock()
	return authDirState.dir
}
