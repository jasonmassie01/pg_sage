//go:build cgo

package sqlast

import (
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// otherStatement is a statement InspectDDL does not type (kind other),
// with what a reviewer still needs: the tables TRUNCATE empties and LOCK
// locks (a part each), and the view CREATE [OR REPLACE] VIEW defines.
func otherStatement(node *pg_query.Node) []DDLStatement {
	switch {
	case node.GetTruncateStmt() != nil:
		tr := node.GetTruncateStmt()
		return relationParts(tr.GetRelations(), DDLStatement{Kind: DDLOther,
			Behavior: behaviorName(tr.GetBehavior())})
	case node.GetLockStmt() != nil:
		lock := node.GetLockStmt()
		return relationParts(lock.GetRelations(), DDLStatement{Kind: DDLOther,
			TableLock: lockModeName(lock.GetMode())})
	case node.GetViewStmt() != nil:
		return []DDLStatement{{Kind: DDLOther, ObjectType: "view",
			Relation: rangeVarName(node.GetViewStmt().GetView())}}
	}
	return []DDLStatement{{Kind: DDLOther}}
}

// relationParts is one copy of base per relation of a statement's list.
func relationParts(relations []*pg_query.Node, base DDLStatement) []DDLStatement {
	if len(relations) == 0 {
		return []DDLStatement{base}
	}
	out := make([]DDLStatement, 0, len(relations))
	for _, rel := range relations {
		p := base
		p.Relation = rangeVarName(rel.GetRangeVar())
		out = append(out, p)
	}
	return out
}

// lockModes are LOCK TABLE's modes by their parser number (lockdefs.h).
var lockModes = map[int32]string{1: "ACCESS SHARE", 2: "ROW SHARE", 3: "ROW EXCLUSIVE",
	4: "SHARE UPDATE EXCLUSIVE", 5: "SHARE", 6: "SHARE ROW EXCLUSIVE", 7: "EXCLUSIVE",
	8: "ACCESS EXCLUSIVE"}

func lockModeName(mode int32) string {
	if m, ok := lockModes[mode]; ok {
		return m
	}
	return "ACCESS EXCLUSIVE"
}

// otherDrop is a DROP of an object that is not a table, view or index: a
// part per object with its type, name and behavior.
func otherDrop(drop *pg_query.DropStmt) []DDLStatement {
	objectType := strings.ToLower(strings.TrimPrefix(drop.GetRemoveType().String(),
		"OBJECT_"))
	base := DDLStatement{Kind: DDLOther, ObjectType: objectType,
		Behavior: behaviorName(drop.GetBehavior()), IfExists: drop.GetMissingOk()}
	if len(drop.GetObjects()) == 0 {
		return []DDLStatement{base}
	}
	out := make([]DDLStatement, 0, len(drop.GetObjects()))
	onTable := onTableObjects[drop.GetRemoveType()]
	for _, object := range drop.GetObjects() {
		p := base
		p.Object, p.Relation = droppedName(object, onTable)
		out = append(out, p)
	}
	return out
}

// onTableObjects are named by the table they are on, then their own name.
var onTableObjects = map[pg_query.ObjectType]bool{pg_query.ObjectType_OBJECT_TRIGGER: true,
	pg_query.ObjectType_OBJECT_RULE: true, pg_query.ObjectType_OBJECT_POLICY: true}

// droppedName is a dropped object's name and, for an object on a table,
// that table.
func droppedName(object *pg_query.Node, onTable bool) (QualifiedName, QualifiedName) {
	switch {
	case object.GetString_() != nil:
		return QualifiedName{Name: object.GetString_().GetSval()}, QualifiedName{}
	case object.GetObjectWithArgs() != nil:
		return nameFromList(object.GetObjectWithArgs().GetObjname()), QualifiedName{}
	case object.GetTypeName() != nil:
		return nameFromList(object.GetTypeName().GetNames()), QualifiedName{}
	}
	list := object.GetList().GetItems()
	last := stringList(list[max(len(list)-1, 0):])
	if !onTable || len(list) < 2 || len(last) != 1 {
		return nameFromList(list), QualifiedName{}
	}
	return QualifiedName{Name: last[0]}, nameFromList(list[:len(list)-1])
}
