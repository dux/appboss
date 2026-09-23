package console

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"dboss/internal/apps"
	"dboss/internal/config"
	"dboss/internal/ops"

	"gopkg.in/yaml.v3"
)

func (h *Handler) configFiles(w http.ResponseWriter, _ *http.Request) {
	files, err := h.store.Files()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

func (h *Handler) configFile(w http.ResponseWriter, r *http.Request) {
	file, err := h.store.Read(r.URL.Query().Get("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, file)
}

func (h *Handler) configValidate(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	var edit configEdit
	if err := decodeJSON(w, r, &edit); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.store.Validate(edit.ID, edit.Contents); err != nil {
		writeJSON(w, http.StatusOK, validateResponse{Error: err.Error(), Line: yamlLine(err)})
		return
	}
	writeJSON(w, http.StatusOK, validateResponse{OK: true})
}

// configWrite saves the file and rescans right away, so the response carries what the change
// did: apps that became invalid and host keys that now wait for a restart.
func (h *Handler) configWrite(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	var edit configEdit
	if err := decodeJSON(w, r, &edit); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := h.service.SaveConfig(session.Email, "config-write", edit.ID, func() (apps.ConfigFile, error) {
		return h.store.Write(edit.ID, edit.Contents, edit.Revision)
	})
	writeConfigResult(w, result, err)
}

func (h *Handler) configLocal(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	var request struct {
		App string `json:"app"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	file, err := h.store.CreateLocal(strings.TrimSpace(request.App))
	if err != nil {
		h.service.Audit(session.Email, request.App, "config-local", request.App, err)
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	h.service.Audit(session.Email, file.App, "config-local", file.ID, nil)
	writeJSON(w, http.StatusOK, file)
}

func (h *Handler) configEffective(w http.ResponseWriter, r *http.Request) {
	contents, err := h.store.Effective(strings.TrimSpace(r.URL.Query().Get("app")))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeText(w, contents)
}

func (h *Handler) configHistory(w http.ResponseWriter, r *http.Request) {
	revisions, err := h.store.History(strings.TrimSpace(r.URL.Query().Get("id")))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revisions": revisions})
}

func (h *Handler) configHistoryFile(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	contents, err := h.store.HistoryContents(strings.TrimSpace(query.Get("id")), strings.TrimSpace(query.Get("revision")))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeText(w, contents)
}

// configRestore writes a saved revision back and rescans, like a normal save.
func (h *Handler) configRestore(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	var edit configEdit
	if err := decodeJSON(w, r, &edit); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := h.service.SaveConfig(session.Email, "config-restore", edit.ID, func() (apps.ConfigFile, error) {
		return h.store.Restore(edit.ID, edit.Revision)
	})
	writeConfigResult(w, result, err)
}

// configForm answers the visual editor: the active file, every value it sets (parsed raw, so a
// $VAR is shown as written), and the recipes for the file's role.
func (h *Handler) configForm(w http.ResponseWriter, r *http.Request) {
	file, err := h.store.Read(strings.TrimSpace(r.URL.Query().Get("id")))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	values, err := parseValues(file.Contents)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	scope := recipeScope(file)
	writeJSON(w, http.StatusOK, map[string]any{"file": file, "values": values, "recipes": recipesForScope(scope)})
}

// configApply is the form's save. It writes the recipe's keys into the server-only override
// (created from the base when missing), validates and rescans like a normal config write, and
// keeps only the paths the recipe declares so the endpoint cannot write an arbitrary key.
func (h *Handler) configApply(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	var request struct {
		ID       string         `json:"id"`
		Revision string         `json:"revision"`
		Recipe   string         `json:"recipe"`
		Values   map[string]any `json:"values"`
		Reset    []string       `json:"reset"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	file, err := h.store.Read(request.ID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	scope := recipeScope(file)
	allowed, ok := recipePaths(request.Recipe, scope)
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown %s recipe %q", scope, request.Recipe))
		return
	}
	values, reset := filterRecipeValues(request.Values, request.Reset, allowed)
	result, err := h.service.SaveConfig(session.Email, "config-apply", file.ID, func() (apps.ConfigFile, error) {
		local, err := h.ensureLocalOverride(file)
		if err != nil {
			return local, err
		}
		contents, err := applyFormPatch(local.Contents, values, reset)
		if err != nil {
			return local, err
		}
		return h.store.Write(request.ID, contents, request.Revision)
	})
	writeConfigResult(w, result, err)
}

// writeConfigResult answers a config save: 409 with the current file on a revision conflict or
// a failed rescan, 422 with the offending line when the file does not validate.
func writeConfigResult(w http.ResponseWriter, result ops.ConfigResult, err error) {
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, result)
	case errors.Is(err, apps.ErrConflict):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "file": result.File})
	case errors.Is(err, ops.ErrRescan):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeJSON(w, http.StatusUnprocessableEntity, validateResponse{Error: err.Error(), Line: yamlLine(err)})
	}
}

// ensureLocalOverride puts the edit into dboss.local.yaml next to the active file, so a deploy
// never overwrites it. The host file and app files have their own helpers.
func (h *Handler) ensureLocalOverride(file apps.ConfigFile) (apps.ConfigFile, error) {
	if file.App != "" {
		return h.store.EnsureLocal(file.App)
	}
	return h.store.CreateHostLocal()
}

// recipeScope reports whether a file carries host keys or app keys.
func recipeScope(file apps.ConfigFile) string {
	if file.App != "" {
		return config.RecipeApp
	}
	return config.RecipeHost
}

// recipesForScope keeps the recipes that belong to a file's role.
func recipesForScope(scope string) []config.Recipe {
	var result []config.Recipe
	for _, recipe := range config.Recipes() {
		if recipe.Scope == scope {
			result = append(result, recipe)
		}
	}
	return result
}

// recipePaths lists the field paths one recipe may write, so a request cannot smuggle in a key
// the recipe does not own.
func recipePaths(id, scope string) (map[string]bool, bool) {
	for _, recipe := range config.Recipes() {
		if recipe.ID != id || recipe.Scope != scope {
			continue
		}
		paths := make(map[string]bool, len(recipe.Fields))
		for _, field := range recipe.Fields {
			paths[field.Path] = true
		}
		return paths, true
	}
	return nil, false
}

// filterRecipeValues keeps only the declared paths. A blank string, empty list or empty map
// means "use the default", so it moves from values to reset and the key is removed from YAML.
func filterRecipeValues(values map[string]any, reset []string, allowed map[string]bool) (map[string]any, []string) {
	clean := make(map[string]any, len(values))
	for path, value := range values {
		if !allowed[path] {
			continue
		}
		if emptyValue(value) {
			reset = append(reset, path)
			continue
		}
		clean[path] = value
	}
	seen := map[string]bool{}
	kept := make([]string, 0, len(reset))
	for _, path := range reset {
		if allowed[path] && !seen[path] {
			seen[path] = true
			kept = append(kept, path)
		}
	}
	return clean, kept
}

func emptyValue(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(typed) == ""
	case []any:
		return len(typed) == 0
	case map[string]any:
		return len(typed) == 0
	}
	return false
}

// parseValues decodes a config file into the generic shape the form reads by dotted path. It
// parses the raw contents, so a $VAR stays literal and is never expanded into a secret.
func parseValues(contents string) (map[string]any, error) {
	values := map[string]any{}
	if strings.TrimSpace(contents) == "" {
		return values, nil
	}
	if err := yaml.Unmarshal([]byte(contents), &values); err != nil {
		return nil, err
	}
	return values, nil
}

func ensureMapping(parent *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value == key {
			if parent.Content[i+1].Kind != yaml.MappingNode {
				parent.Content[i+1] = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			}
			return parent.Content[i+1]
		}
	}
	node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	parent.Content = append(parent.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, node)
	return node
}

func setMapping(parent *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value == key {
			parent.Content[i+1] = value
			return
		}
	}
	parent.Content = append(parent.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}

var yamlLinePattern = regexp.MustCompile(`line (\d+)`)

// yamlLine finds the line an error points at so the editor can highlight it.
func yamlLine(err error) int {
	var cfgErr *config.Error
	if errors.As(err, &cfgErr) && cfgErr.Line > 0 {
		return cfgErr.Line
	}
	match := yamlLinePattern.FindStringSubmatch(err.Error())
	if match == nil {
		return 0
	}
	line, _ := strconv.Atoi(match[1])
	return line
}
