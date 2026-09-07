package vectorlab

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
)

type planNode struct {
	NodeType     string     `json:"Node Type"`
	IndexName    string     `json:"Index Name"`
	Schema       string     `json:"Schema"`
	RelationName string     `json:"Relation Name"`
	Plans        []planNode `json:"Plans"`
}

func (s *postgresSource) loadIndexes(ctx context.Context) (map[string]bool, error) {
	rows, err := s.tx.Query(ctx, `SELECT i.relname FROM pg_catalog.pg_index x
		JOIN pg_catalog.pg_class t ON t.oid=x.indrelid
		JOIN pg_catalog.pg_namespace n ON n.oid=t.relnamespace
		JOIN pg_catalog.pg_class i ON i.oid=x.indexrelid
		JOIN pg_catalog.pg_am am ON am.oid=i.relam
		WHERE n.nspname=$1 AND t.relname=$2 AND am.amname='hnsw'
		AND x.indisvalid AND x.indisready`, s.manifest.Schema, s.manifest.Table)
	if err != nil {
		return nil, safeError("inspect HNSW indexes", err)
	}
	defer rows.Close()
	indexes := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, safeError("read index metadata", err)
		}
		indexes[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, safeError("read index metadata", err)
	}
	return indexes, nil
}

func (s *postgresSource) planIndexes(
	ctx context.Context, sql string, args []any,
) ([]string, error) {
	var raw []byte
	err := s.tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON, VERBOSE) "+sql, args...).Scan(&raw)
	if err != nil {
		return nil, safeError("inspect vector query plan", err)
	}
	var plan []struct {
		Plan planNode `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plan); err != nil || len(plan) != 1 {
		return nil, errors.New("inspect vector query plan: malformed PostgreSQL plan")
	}
	indexes := make(map[string]bool)
	s.collectIndexes(plan[0].Plan, indexes)
	result := make([]string, 0, len(indexes))
	for index := range indexes {
		result = append(result, index)
	}
	sort.Strings(result)
	return result, nil
}

func (s *postgresSource) collectIndexes(node planNode, indexes map[string]bool) {
	if node.NodeType == "Index Scan" && node.Schema == s.manifest.Schema &&
		node.RelationName == s.manifest.Table && s.hnswIndexes[node.IndexName] {
		indexes[node.IndexName] = true
	}
	for _, child := range node.Plans {
		s.collectIndexes(child, indexes)
	}
}
