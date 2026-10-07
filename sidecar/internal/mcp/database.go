package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// DatabaseRef is one monitored database: its fleet name and, in meta-db
// mode, its numeric id (0 when the fleet has none).
type DatabaseRef struct {
	Name string
	ID   int64
}

// Directory lists the monitored databases the server validates the
// `database` argument against.
type Directory interface {
	Databases() []DatabaseRef
}

// bindDatabase resolves the `database` argument of a call: the named
// database, the database of a legacy database_id, or the only monitored
// database. The resolved name is written back into the arguments and the
// context. Refusals are distinguishable: not permitted for this principal
// (whether or not it exists), unknown, or required.
func (s *Server) bindDatabase(ctx context.Context, tool string,
	arguments json.RawMessage) (context.Context, json.RawMessage, *rpcError) {
	if fleetWideTool(tool) {
		return ctx, arguments, nil
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(arguments, &object) != nil {
		return ctx, nil, failure(codeInvalidParams, "tool arguments must be a JSON object")
	}
	name, id, failed := databaseArguments(object)
	if failed != nil {
		return ctx, nil, failed
	}
	ref, failed := s.resolveDatabase(ctx, name, id)
	if failed != nil || ref.Name == "" {
		return ctx, arguments, failed
	}
	object["database"], _ = json.Marshal(ref.Name)
	if legacyDatabaseIDTools[tool] && ref.ID != 0 {
		object["database_id"], _ = json.Marshal(ref.ID)
	}
	bound, err := json.Marshal(object)
	if err != nil {
		return ctx, nil, failure(codeInternal, "internal error")
	}
	return WithDatabase(ctx, ref.Name), bound, nil
}

// databaseArguments reads `database` (a string) and the legacy integer
// `database_id`; nil pointers mean absent.
func databaseArguments(object map[string]json.RawMessage) (*string, *int64, *rpcError) {
	var name *string
	var id *int64
	if raw, ok := object["database"]; ok && string(raw) != "null" {
		var value string
		if json.Unmarshal(raw, &value) != nil {
			return nil, nil, failure(codeInvalidParams, "invalid arguments: database "+
				"must be a string")
		}
		name = &value
	}
	if raw, ok := object["database_id"]; ok && string(raw) != "null" {
		var value int64
		if json.Unmarshal(raw, &value) != nil {
			return nil, nil, failure(codeInvalidParams, "invalid arguments: database_id "+
				"must be an integer")
		}
		id = &value
	}
	return name, id, nil
}

func (s *Server) resolveDatabase(ctx context.Context, name *string,
	id *int64) (DatabaseRef, *rpcError) {
	p, bound := PrincipalFromContext(ctx)
	restricted := bound && p.Databases != nil
	if name != nil && restricted && !p.MayUseDatabase(*name) {
		return DatabaseRef{}, notPermitted(*name)
	}
	if s.directory == nil {
		if name == nil && restricted {
			return DatabaseRef{}, s.databaseRequired(ctx)
		}
		if name == nil {
			return DatabaseRef{}, nil
		}
		return DatabaseRef{Name: *name}, nil
	}
	ref, failed := s.lookupDatabase(ctx, name, id)
	if failed != nil {
		return DatabaseRef{}, failed
	}
	if restricted && !p.MayUseDatabase(ref.Name) {
		return DatabaseRef{}, notPermitted(ref.Name)
	}
	return ref, nil
}

func (s *Server) lookupDatabase(ctx context.Context, name *string,
	id *int64) (DatabaseRef, *rpcError) {
	refs := s.directory.Databases()
	switch {
	case name != nil:
		for _, ref := range refs {
			if ref.Name == *name {
				if id != nil && ref.ID != *id {
					return DatabaseRef{}, failure(codeInvalidParams, "invalid arguments: "+
						"database and database_id name different databases")
				}
				return ref, nil
			}
		}
		return DatabaseRef{}, s.unknownDatabase(ctx, *name)
	case id != nil:
		if ref, ok := databaseByID(refs, *id); ok {
			return ref, nil
		}
		return DatabaseRef{}, s.unknownDatabase(ctx, fmt.Sprint(*id))
	case len(refs) == 1:
		return refs[0], nil
	}
	return DatabaseRef{}, s.databaseRequired(ctx)
}

// databaseByID resolves a non-zero id that exactly one database carries.
func databaseByID(refs []DatabaseRef, id int64) (DatabaseRef, bool) {
	var found []DatabaseRef
	for _, ref := range refs {
		if id != 0 && ref.ID == id {
			found = append(found, ref)
		}
	}
	if len(found) != 1 {
		return DatabaseRef{}, false
	}
	return found[0], true
}

func notPermitted(name string) *rpcError {
	return failure(codeNotPermitted, fmt.Sprintf("database %q is not permitted for this "+
		"principal", printableName(name)))
}

func (s *Server) unknownDatabase(ctx context.Context, name string) *rpcError {
	return failure(codeUnknownDatabase, fmt.Sprintf("unknown database %q; monitored: %s",
		printableName(name), s.namesText(ctx)))
}

func (s *Server) databaseRequired(ctx context.Context) *rpcError {
	return failure(codeDatabaseRequired, "database is required (more than one database "+
		"is monitored): name one of "+s.namesText(ctx)+" (see list_databases)")
}

func (s *Server) namesText(ctx context.Context) string {
	names := s.permittedDatabaseNames(ctx)
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// permittedDatabaseNames are the monitored databases the caller may name,
// sorted; nil without a directory.
func (s *Server) permittedDatabaseNames(ctx context.Context) []string {
	if s.directory == nil {
		return nil
	}
	p, bound := PrincipalFromContext(ctx)
	names := []string{}
	for _, ref := range s.directory.Databases() {
		if !bound || p.MayUseDatabase(ref.Name) {
			names = append(names, ref.Name)
		}
	}
	sort.Strings(names)
	return names
}

func (s *Server) listDatabases(ctx context.Context, _ string,
	arguments json.RawMessage) (any, *rpcError) {
	var none struct{}
	if !decodeStrict(arguments, &none) {
		return nil, failure(codeInvalidParams, "invalid arguments: list_databases takes "+
			"no arguments")
	}
	out := []map[string]any{}
	single := false
	if s.directory != nil {
		refs := s.directory.Databases()
		single = len(refs) == 1
		permitted := map[string]bool{}
		for _, name := range s.permittedDatabaseNames(ctx) {
			permitted[name] = true
		}
		sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
		for _, ref := range refs {
			if permitted[ref.Name] {
				out = append(out, map[string]any{"name": ref.Name, "database_id": ref.ID,
					"default": single})
			}
		}
	}
	return toolSuccess(map[string]any{"databases": out, "single_database_mode": single}), nil
}
