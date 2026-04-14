package graph

import "time"

type NodeKind string

const (
	NodeFile     NodeKind = "File"
	NodeClass    NodeKind = "Class"
	NodeFunction NodeKind = "Function"
	NodeType     NodeKind = "Type"
	NodeTest     NodeKind = "Test"
)

type EdgeKind string

const (
	EdgeCalls       EdgeKind = "CALLS"
	EdgeImportsFrom EdgeKind = "IMPORTS_FROM"
	EdgeInherits    EdgeKind = "INHERITS"
	EdgeImplements  EdgeKind = "IMPLEMENTS"
	EdgeContains    EdgeKind = "CONTAINS"
	EdgeTestedBy    EdgeKind = "TESTED_BY"
	EdgeDependsOn   EdgeKind = "DEPENDS_ON"
	EdgeReferences  EdgeKind = "REFERENCES"
)

type NodeInfo struct {
	Kind       string
	Name       string
	FilePath   string
	LineStart  int
	LineEnd    int
	Language   string
	ParentName string
	Params     string
	ReturnType string
	Modifiers  string
	IsTest     bool
	Extra      map[string]any
}

type EdgeInfo struct {
	Kind     string
	Source   string
	Target   string
	FilePath string
	Line     int
	Extra    map[string]any
}

type GraphNode struct {
	ID            int64
	Kind          string
	Name          string
	QualifiedName string
	FilePath      string
	LineStart     int
	LineEnd       int
	Language      string
	ParentName    *string
	Params        *string
	ReturnType    *string
	IsTest        bool
	FileHash      *string
	Extra         map[string]any
}

type GraphEdge struct {
	ID              int64
	Kind            string
	SourceQualified string
	TargetQualified string
	FilePath        string
	Line            int
	Extra           map[string]any
}

type GraphStats struct {
	TotalNodes  int            `json:"total_nodes"`
	TotalEdges  int            `json:"total_edges"`
	NodesByKind map[string]int `json:"nodes_by_kind"`
	EdgesByKind map[string]int `json:"edges_by_kind"`
	Languages   []string       `json:"languages"`
	FilesCount  int            `json:"files_count"`
	LastUpdated string         `json:"last_updated"`
}

type ImpactResult struct {
	ChangedNodes  []GraphNode `json:"changed_nodes"`
	ImpactedNodes []GraphNode `json:"impacted_nodes"`
	ImpactedFiles []string    `json:"impacted_files"`
	Edges         []GraphEdge `json:"edges"`
	Truncated     bool        `json:"truncated"`
	TotalImpacted int         `json:"total_impacted"`
}

func nowUnix() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}
