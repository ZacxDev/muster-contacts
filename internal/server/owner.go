package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/ZacxDev/muster-contacts/internal/store"
)

// The owner API (design: "The owner UI"), every route behind owner():
//
//	GET    /api/contacts?q=&cursor=     the list, newest activity first
//	POST   /api/contacts                Add: {name, emails, note}
//	GET    /api/contacts/{id}           one contact, whole
//	PATCH  /api/contacts/{id}           Edit: any of {name, emails, note}
//	DELETE /api/contacts/{id}           Delete (for real)
//	POST   /api/contacts/{id}/merge     {from}: from's everything moves to id
//	GET    /api/contacts/{id}/export    Download: every row about them
//	GET    /api/export                  Download all
//	DELETE /api/all                     Delete everything: {"confirm": "delete everything"}
//	GET    /api/settings, PUT /api/settings {collect}
//	GET    /api/alive                   204 while the session lives (the page's 30 s poll)
func (s *Server) ownerRoutes() {
	s.mux.HandleFunc("GET /api/contacts", s.owner(s.handleList))
	s.mux.HandleFunc("POST /api/contacts", s.owner(s.handleCreate))
	s.mux.HandleFunc("GET /api/contacts/{id}", s.owner(s.handleGet))
	s.mux.HandleFunc("PATCH /api/contacts/{id}", s.owner(s.handleUpdate))
	s.mux.HandleFunc("DELETE /api/contacts/{id}", s.owner(s.handleDelete))
	s.mux.HandleFunc("POST /api/contacts/{id}/merge", s.owner(s.handleMerge))
	s.mux.HandleFunc("GET /api/contacts/{id}/export", s.owner(s.handleExportOne))
	s.mux.HandleFunc("GET /api/export", s.owner(s.handleExportAll))
	s.mux.HandleFunc("DELETE /api/all", s.owner(s.handleDeleteAll))
	s.mux.HandleFunc("GET /api/settings", s.owner(s.handleSettings))
	s.mux.HandleFunc("PUT /api/settings", s.owner(s.handleSetSettings))
	s.mux.HandleFunc("GET /api/alive", s.owner(func(w http.ResponseWriter, r *http.Request, o *store.Owner) {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	}))
}

// ownerBodyMax is the owner API's largest body.
const ownerBodyMax = 16 << 10

// DeleteEverything is the typed confirmation Delete everything needs.
const DeleteEverything = "delete everything"

// decodeOwner reads one JSON object into v, refusing an unknown field.
func decodeOwner(w http.ResponseWriter, r *http.Request, v any) bool {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, ownerBodyMax))
	if err == nil {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err = dec.Decode(v); err == nil && dec.More() {
			err = errors.New("trailing data")
		}
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the body is not this route's JSON object"})
		return false
	}
	return true
}

// writeStoreErr answers a store error: a refused field (400, naming it), an
// email another contact holds (409, with that contact for Merge), a contact
// that is not this owner's (404, the same as an unknown one), or 500.
func (s *Server) writeStoreErr(w http.ResponseWriter, what string, err error) {
	var fe store.FieldError
	var taken store.ErrEmailTaken
	switch {
	case errors.As(err, &fe):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid field", "field": fe.Field, "index": fe.Index})
	case errors.As(err, &taken):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "another contact has this email",
			"conflict": map[string]any{"id": taken.ContactID, "name": taken.Name, "address": taken.Address}})
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such contact"})
	default:
		// The error's text is the database's (a constraint or an I/O
		// failure), never a value; it is still not logged, only its kind.
		s.cfg.Log.Printf("owner api: %s failed", what)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the request could not be completed"})
	}
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request, o *store.Owner) {
	q := r.URL.Query()
	var after *store.Cursor
	if v := q.Get("cursor"); v != "" {
		c, ok := store.ParseCursor(v)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "cursor is the next value of an earlier page"})
			return
		}
		after = &c
	}
	rows, next, err := o.List(r.Context(), q.Get("q"), after, store.ListLimit)
	if err != nil {
		s.writeStoreErr(w, "list", err)
		return
	}
	var nextS *string
	if next != nil {
		v := next.String()
		nextS = &v
	}
	writeJSON(w, http.StatusOK, map[string]any{"contacts": rows, "next": nextS})
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request, o *store.Owner) {
	var in store.ContactIn
	if !decodeOwner(w, r, &in) {
		return
	}
	c, err := o.Create(r.Context(), in, s.cfg.Now())
	if err != nil {
		s.writeStoreErr(w, "create", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"contact": c})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request, o *store.Owner) {
	c, err := o.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeStoreErr(w, "get", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"contact": c})
}

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request, o *store.Owner) {
	var p store.ContactPatch
	if !decodeOwner(w, r, &p) {
		return
	}
	c, err := o.Update(r.Context(), r.PathValue("id"), p, s.cfg.Now())
	if err != nil {
		s.writeStoreErr(w, "update", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"contact": c})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request, o *store.Owner) {
	if err := o.Delete(r.Context(), r.PathValue("id"), s.cfg.Now()); err != nil {
		s.writeStoreErr(w, "delete", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMerge(w http.ResponseWriter, r *http.Request, o *store.Owner) {
	var in struct {
		From string `json:"from"`
	}
	if !decodeOwner(w, r, &in) {
		return
	}
	c, err := o.Merge(r.Context(), r.PathValue("id"), in.From, s.cfg.Now())
	if err != nil {
		s.writeStoreErr(w, "merge", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"contact": c})
}

// download answers v as a JSON file the browser saves.
func download(w http.ResponseWriter, name string, v any) {
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleExportOne(w http.ResponseWriter, r *http.Request, o *store.Owner) {
	c, err := o.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeStoreErr(w, "export", err)
		return
	}
	download(w, "contact.json", map[string]any{"contact": c, "exported_at": s.cfg.Now().UTC()})
}

func (s *Server) handleExportAll(w http.ResponseWriter, r *http.Request, o *store.Owner) {
	cs, err := o.Export(r.Context())
	if err != nil {
		s.writeStoreErr(w, "export", err)
		return
	}
	download(w, "contacts.json", map[string]any{"contacts": cs, "exported_at": s.cfg.Now().UTC()})
}

func (s *Server) handleDeleteAll(w http.ResponseWriter, r *http.Request, o *store.Owner) {
	var in struct {
		Confirm string `json:"confirm"`
	}
	if !decodeOwner(w, r, &in) {
		return
	}
	if in.Confirm != DeleteEverything {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid field", "field": "confirm"})
		return
	}
	if err := o.DeleteAll(r.Context(), s.cfg.Now()); err != nil {
		s.writeStoreErr(w, "delete all", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request, o *store.Owner) {
	st, err := o.Settings(r.Context())
	if err != nil {
		s.writeStoreErr(w, "settings", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleSetSettings(w http.ResponseWriter, r *http.Request, o *store.Owner) {
	var in struct {
		Collect *bool `json:"collect"`
	}
	if !decodeOwner(w, r, &in) {
		return
	}
	if in.Collect == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid field", "field": "collect"})
		return
	}
	if err := o.SetCollect(r.Context(), *in.Collect, s.cfg.Now()); err != nil {
		s.writeStoreErr(w, "settings", err)
		return
	}
	writeJSON(w, http.StatusOK, store.Settings{Collect: *in.Collect})
}
