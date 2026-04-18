// Package hitlpolicyapi provides CRUD HTTP routes for named HITL policy files stored in VFS.
// Policies are JSON files (hitlservice.Policy) co-located with chains in the project VFS.
package hitlpolicyapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/contenox/contenox/apiframework"
	"github.com/contenox/contenox/apiframework/middleware"
	"github.com/contenox/contenox/hitlservice"
	"github.com/contenox/contenox/taskchainservice"
	"github.com/contenox/contenox/vfsservice"
)

// AddRoutes registers HITL policy CRUD on mux.
// GET  /hitl-policies/list     — list policy filenames in VFS
// GET  /hitl-policies?name=... — read a policy
// POST /hitl-policies?name=... — create a new policy
// PUT  /hitl-policies?name=... — update an existing policy
// DELETE /hitl-policies?name=. — delete a policy
func AddRoutes(mux *http.ServeMux, vfs vfsservice.Service, auth middleware.AuthZReader) {
	h := &handler{vfs: vfs, auth: auth}
	mux.HandleFunc("GET /hitl-policies/list", h.listPolicies)
	mux.HandleFunc("GET /hitl-policies", h.getPolicy)
	mux.HandleFunc("POST /hitl-policies", h.createPolicy)
	mux.HandleFunc("PUT /hitl-policies", h.updatePolicy)
	mux.HandleFunc("DELETE /hitl-policies", h.deletePolicy)
}

type handler struct {
	vfs  vfsservice.Service
	auth middleware.AuthZReader
}

func normalizePolicyName(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("%w: query parameter name is required", apiframework.ErrBadRequest)
	}
	name, err := taskchainservice.NormalizeVFSPath(raw)
	if err != nil {
		return "", fmt.Errorf("%w: %s", apiframework.ErrBadRequest, err.Error())
	}
	if !strings.HasSuffix(name, ".json") {
		return "", fmt.Errorf("%w: policy name must end in .json", apiframework.ErrBadRequest)
	}
	return name, nil
}

// Lists the filenames of all HITL policy JSON files in the VFS root.
func (h *handler) listPolicies(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, err := h.auth.GetIdentity(ctx); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.AuthorizeOperation)
		return
	}
	files, err := h.vfs.GetFilesByPath(ctx, "")
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.ListOperation)
		return
	}
	names := []string{}
	for _, f := range files {
		if strings.HasPrefix(f.Name, "hitl-policy") && strings.HasSuffix(f.Name, ".json") {
			names = append(names, f.Name)
		}
	}
	_ = apiframework.Encode(w, r, http.StatusOK, names) // @response []string
}

// Retrieves a HITL policy at the given name.
func (h *handler) getPolicy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, err := h.auth.GetIdentity(ctx); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.AuthorizeOperation)
		return
	}
	name, err := normalizePolicyName(apiframework.GetQueryParam(r, "name", "", "Policy filename (e.g. hitl-policy-strict.json)."))
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.GetOperation)
		return
	}
	f, err := h.vfs.GetFileByID(ctx, name)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.GetOperation)
		return
	}
	var policy hitlservice.Policy
	if err := json.Unmarshal(f.Data, &policy); err != nil {
		_ = apiframework.Error(w, r, fmt.Errorf("invalid policy JSON: %w", err), apiframework.GetOperation)
		return
	}
	_ = apiframework.Encode(w, r, http.StatusOK, policy) // @response hitlservice.Policy
}

// Creates a new HITL policy file at name (must not exist).
func (h *handler) createPolicy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, err := h.auth.GetIdentity(ctx); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.AuthorizeOperation)
		return
	}
	name, err := normalizePolicyName(apiframework.GetQueryParam(r, "name", "", "Policy filename (e.g. hitl-policy-custom.json)."))
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}
	policy, err := apiframework.Decode[hitlservice.Policy](r) // @request hitlservice.Policy
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}
	data, err := json.Marshal(policy)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}
	if _, err := h.vfs.CreateFile(ctx, &vfsservice.File{Name: name, Data: data, ContentType: "application/json"}); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}
	_ = apiframework.Encode(w, r, http.StatusCreated, policy) // @response hitlservice.Policy
}

// Updates an existing HITL policy file at name.
func (h *handler) updatePolicy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, err := h.auth.GetIdentity(ctx); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.AuthorizeOperation)
		return
	}
	name, err := normalizePolicyName(apiframework.GetQueryParam(r, "name", "", "Policy filename (e.g. hitl-policy-strict.json)."))
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}
	policy, err := apiframework.Decode[hitlservice.Policy](r) // @request hitlservice.Policy
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}
	data, err := json.Marshal(policy)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}
	prev, err := h.vfs.GetFileByID(ctx, name)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}
	prev.Data = data
	prev.ContentType = "application/json"
	if _, err := h.vfs.UpdateFile(ctx, prev); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}
	_ = apiframework.Encode(w, r, http.StatusOK, policy) // @response hitlservice.Policy
}

// Deletes the HITL policy file at name.
func (h *handler) deletePolicy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, err := h.auth.GetIdentity(ctx); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.AuthorizeOperation)
		return
	}
	name, err := normalizePolicyName(apiframework.GetQueryParam(r, "name", "", "Policy filename (e.g. hitl-policy-custom.json)."))
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.DeleteOperation)
		return
	}
	if err := h.vfs.DeleteFile(ctx, name); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.DeleteOperation)
		return
	}
	_ = apiframework.Encode(w, r, http.StatusOK, fmt.Sprintf("policy %s deleted", name)) // @response string
}
