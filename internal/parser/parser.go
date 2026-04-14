package parser

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"

	"github.com/harshsharma/code-review-graph-go/internal/graph"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/bash"
	"github.com/smacker/go-tree-sitter/c"
	"github.com/smacker/go-tree-sitter/cpp"
	"github.com/smacker/go-tree-sitter/csharp"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/java"
	tsjs "github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/kotlin"
	"github.com/smacker/go-tree-sitter/lua"
	"github.com/smacker/go-tree-sitter/php"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/ruby"
	"github.com/smacker/go-tree-sitter/rust"
	"github.com/smacker/go-tree-sitter/scala"
	"github.com/smacker/go-tree-sitter/swift"
	"github.com/smacker/go-tree-sitter/typescript/tsx"
	"github.com/smacker/go-tree-sitter/typescript/typescript"
)

var tsLanguages = map[string]*sitter.Language{
	"python":     python.GetLanguage(),
	"javascript": tsjs.GetLanguage(),
	"typescript": typescript.GetLanguage(),
	"tsx":        tsx.GetLanguage(),
	"go":         golang.GetLanguage(),
	"rust":       rust.GetLanguage(),
	"java":       java.GetLanguage(),
	"c":          c.GetLanguage(),
	"cpp":        cpp.GetLanguage(),
	"csharp":     csharp.GetLanguage(),
	"ruby":       ruby.GetLanguage(),
	"kotlin":     kotlin.GetLanguage(),
	"swift":      swift.GetLanguage(),
	"php":        php.GetLanguage(),
	"scala":      scala.GetLanguage(),
	"lua":        lua.GetLanguage(),
	"bash":       bash.GetLanguage(),
}

type ParseResult struct {
	FilePath string
	Nodes    []graph.NodeInfo
	Edges    []graph.EdgeInfo
	FileHash string
	Err      error
}

type CodeParser struct {
	mu      sync.Mutex
	parsers map[string]*sitter.Parser
}

func NewCodeParser() *CodeParser {
	return &CodeParser{
		parsers: make(map[string]*sitter.Parser),
	}
}

func (cp *CodeParser) getParser(lang string) (*sitter.Parser, error) {
	cp.mu.Lock()
	defer cp.mu.Unlock()

	if p, ok := cp.parsers[lang]; ok {
		return p, nil
	}

	tsLang, ok := tsLanguages[lang]
	if !ok {
		return nil, fmt.Errorf("unsupported language: %s", lang)
	}

	p := sitter.NewParser()
	p.SetLanguage(tsLang)
	cp.parsers[lang] = p
	return p, nil
}

func DetectLanguage(filePath string) string {
	ext := strings.ToLower(filepath.Ext(filePath))
	if lang, ok := ExtensionToLanguage[ext]; ok {
		return lang
	}
	return ""
}

func FileHash(data []byte) string {
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h)
}

func (cp *CodeParser) ParseBytes(ctx context.Context, filePath string, source []byte) ([]graph.NodeInfo, []graph.EdgeInfo, error) {
	lang := DetectLanguage(filePath)
	if lang == "" {
		return nil, nil, fmt.Errorf("unsupported file type: %s", filePath)
	}

	parser, err := cp.getParser(lang)
	if err != nil {
		return nil, nil, err
	}

	cp.mu.Lock()
	tree, err := parser.ParseCtx(ctx, nil, source)
	cp.mu.Unlock()
	if err != nil {
		return nil, nil, fmt.Errorf("parsing %s: %w", filePath, err)
	}

	rootNode := tree.RootNode()
	if rootNode == nil {
		return nil, nil, fmt.Errorf("nil root node for %s", filePath)
	}

	var nodes []graph.NodeInfo
	var edges []graph.EdgeInfo

	// File-level node
	lineCount := int(rootNode.EndPoint().Row) + 1
	nodes = append(nodes, graph.NodeInfo{
		Kind:     string(graph.NodeFile),
		Name:     filepath.Base(filePath),
		FilePath: filePath,
		LineStart: 1,
		LineEnd:   lineCount,
		Language:  lang,
	})

	classTypes := toSet(ClassTypes[lang])
	functionTypes := toSet(FunctionTypes[lang])
	importTypes := toSet(ImportTypes[lang])

	cp.walkTree(rootNode, filePath, lang, source, "", classTypes, functionTypes, importTypes, &nodes, &edges)

	return nodes, edges, nil
}

func (cp *CodeParser) walkTree(
	node *sitter.Node,
	filePath, lang string,
	source []byte,
	parentClass string,
	classTypes, functionTypes, importTypes map[string]bool,
	nodes *[]graph.NodeInfo,
	edges *[]graph.EdgeInfo,
) {
	if node == nil {
		return
	}

	nodeType := node.Type()

	if classTypes[nodeType] {
		name := extractName(node, lang, "class", source)
		if name != "" {
			lineStart := int(node.StartPoint().Row) + 1
			lineEnd := int(node.EndPoint().Row) + 1
			*nodes = append(*nodes, graph.NodeInfo{
				Kind:      string(graph.NodeClass),
				Name:      name,
				FilePath:  filePath,
				LineStart: lineStart,
				LineEnd:   lineEnd,
				Language:  lang,
			})

			*edges = append(*edges, graph.EdgeInfo{
				Kind:     string(graph.EdgeContains),
				Source:   filePath,
				Target:   fmt.Sprintf("%s::%s", filePath, name),
				FilePath: filePath,
				Line:     lineStart,
			})

			// Recurse into class body with this class as parent
			for i := 0; i < int(node.ChildCount()); i++ {
				child := node.Child(i)
				cp.walkTree(child, filePath, lang, source, name, classTypes, functionTypes, importTypes, nodes, edges)
			}
			return
		}
	}

	if functionTypes[nodeType] {
		name := extractName(node, lang, "function", source)
		if name != "" {
			lineStart := int(node.StartPoint().Row) + 1
			lineEnd := int(node.EndPoint().Row) + 1

			isTest := isTestFunction(name, lang, filePath)
			kind := string(graph.NodeFunction)
			if isTest {
				kind = string(graph.NodeTest)
			}

			params := extractParams(node, lang, source)
			returnType := extractReturnType(node, lang, source)

			ni := graph.NodeInfo{
				Kind:       kind,
				Name:       name,
				FilePath:   filePath,
				LineStart:  lineStart,
				LineEnd:    lineEnd,
				Language:   lang,
				ParentName: parentClass,
				Params:     params,
				ReturnType: returnType,
				IsTest:     isTest,
			}
			*nodes = append(*nodes, ni)

			parentQN := filePath
			if parentClass != "" {
				parentQN = fmt.Sprintf("%s::%s", filePath, parentClass)
			}
			qualified := fmt.Sprintf("%s::%s", filePath, name)
			if parentClass != "" {
				qualified = fmt.Sprintf("%s::%s.%s", filePath, parentClass, name)
			}

			*edges = append(*edges, graph.EdgeInfo{
				Kind:     string(graph.EdgeContains),
				Source:   parentQN,
				Target:   qualified,
				FilePath: filePath,
				Line:     lineStart,
			})

			extractCalls(node, filePath, lang, source, qualified, edges)
		}
	}

	if importTypes[nodeType] {
		extractImport(node, filePath, lang, source, edges)
	}

	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		cp.walkTree(child, filePath, lang, source, parentClass, classTypes, functionTypes, importTypes, nodes, edges)
	}
}

func extractName(node *sitter.Node, lang, kind string, source []byte) string {
	// Go type declarations have a different structure
	if lang == "go" && kind == "class" {
		for i := 0; i < int(node.ChildCount()); i++ {
			child := node.Child(i)
			if child.Type() == "type_spec" {
				nameNode := child.ChildByFieldName("name")
				if nameNode != nil {
					return nameNode.Content(source)
				}
			}
		}
		return ""
	}

	nameNode := node.ChildByFieldName("name")
	if nameNode != nil {
		return nameNode.Content(source)
	}

	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "identifier" || child.Type() == "type_identifier" ||
			child.Type() == "property_identifier" {
			return child.Content(source)
		}
	}
	return ""
}

func extractParams(node *sitter.Node, _ string, source []byte) string {
	paramsNode := node.ChildByFieldName("parameters")
	if paramsNode == nil {
		paramsNode = node.ChildByFieldName("formal_parameters")
	}
	if paramsNode == nil {
		for i := 0; i < int(node.ChildCount()); i++ {
			child := node.Child(i)
			t := child.Type()
			if t == "parameters" || t == "formal_parameters" || t == "parameter_list" {
				paramsNode = child
				break
			}
		}
	}
	if paramsNode != nil {
		content := paramsNode.Content(source)
		if len(content) > 500 {
			content = content[:500]
		}
		return content
	}
	return ""
}

func extractReturnType(node *sitter.Node, _ string, source []byte) string {
	retNode := node.ChildByFieldName("return_type")
	if retNode == nil {
		retNode = node.ChildByFieldName("result")
	}
	if retNode != nil {
		return retNode.Content(source)
	}
	return ""
}

func extractCalls(node *sitter.Node, filePath, lang string, source []byte, callerQN string, edges *[]graph.EdgeInfo) {
	if node == nil {
		return
	}

	nodeType := node.Type()
	if nodeType == "call_expression" || nodeType == "call" || nodeType == "invocation_expression" {
		funcNode := node.ChildByFieldName("function")
		if funcNode == nil {
			funcNode = node.ChildByFieldName("name")
		}
		if funcNode == nil && node.ChildCount() > 0 {
			funcNode = node.Child(0)
		}
		if funcNode != nil {
			callName := funcNode.Content(source)
			if callName != "" && len(callName) < 200 {
				lineNum := int(node.StartPoint().Row) + 1
				*edges = append(*edges, graph.EdgeInfo{
					Kind:     string(graph.EdgeCalls),
					Source:   callerQN,
					Target:   callName,
					FilePath: filePath,
					Line:     lineNum,
				})
			}
		}
	}

	for i := 0; i < int(node.ChildCount()); i++ {
		extractCalls(node.Child(i), filePath, lang, source, callerQN, edges)
	}
}

func extractImport(node *sitter.Node, filePath, lang string, source []byte, edges *[]graph.EdgeInfo) {
	content := node.Content(source)
	lineNum := int(node.StartPoint().Row) + 1

	var target string
	switch lang {
	case "python":
		target = extractPythonImport(content)
	case "go":
		target = extractGoImport(content)
	case "javascript", "typescript", "tsx":
		target = extractJSImport(content)
	case "java":
		target = extractJavaImport(content)
	case "rust":
		target = extractRustImport(content)
	case "c", "cpp":
		target = extractCInclude(content)
	default:
		target = content
	}

	if target != "" {
		*edges = append(*edges, graph.EdgeInfo{
			Kind:     string(graph.EdgeImportsFrom),
			Source:   filePath,
			Target:   target,
			FilePath: filePath,
			Line:     lineNum,
		})
	}
}

func extractPythonImport(content string) string {
	content = strings.TrimSpace(content)
	if strings.HasPrefix(content, "from ") {
		parts := strings.Fields(content)
		if len(parts) >= 2 {
			return parts[1]
		}
	}
	if strings.HasPrefix(content, "import ") {
		parts := strings.Fields(content)
		if len(parts) >= 2 {
			return parts[1]
		}
	}
	return ""
}

func extractGoImport(content string) string {
	content = strings.TrimSpace(content)
	content = strings.Trim(content, "\"")
	if idx := strings.Index(content, "\""); idx >= 0 {
		rest := content[idx+1:]
		if end := strings.Index(rest, "\""); end >= 0 {
			return rest[:end]
		}
	}
	return ""
}

func extractJSImport(content string) string {
	for _, delim := range []string{"'", "\""} {
		idx := strings.LastIndex(content, delim)
		if idx < 0 {
			continue
		}
		sub := content[:idx]
		start := strings.LastIndex(sub, delim)
		if start >= 0 {
			return sub[start+1:]
		}
	}
	return ""
}

func extractJavaImport(content string) string {
	content = strings.TrimPrefix(content, "import ")
	content = strings.TrimSuffix(content, ";")
	content = strings.TrimSpace(content)
	return content
}

func extractRustImport(content string) string {
	content = strings.TrimPrefix(content, "use ")
	content = strings.TrimSuffix(content, ";")
	content = strings.TrimSpace(content)
	return content
}

func extractCInclude(content string) string {
	content = strings.TrimSpace(content)
	for _, pair := range []struct{ open, close string }{
		{"\"", "\""}, {"<", ">"},
	} {
		start := strings.Index(content, pair.open)
		if start < 0 {
			continue
		}
		rest := content[start+1:]
		end := strings.Index(rest, pair.close)
		if end >= 0 {
			return rest[:end]
		}
	}
	return ""
}

func isTestFunction(name, lang, filePath string) bool {
	if prefixes, ok := TestPrefixes[lang]; ok {
		for _, prefix := range prefixes {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
	}
	// Check file path for test patterns
	base := filepath.Base(filePath)
	testPatterns := []string{"_test.", ".test.", ".spec.", "test_"}
	for _, pattern := range testPatterns {
		if strings.Contains(base, pattern) {
			return true
		}
	}
	return false
}

func toSet(slice []string) map[string]bool {
	m := make(map[string]bool, len(slice))
	for _, s := range slice {
		m[s] = true
	}
	return m
}

// Close releases all parser resources
func (cp *CodeParser) Close() {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	for _, p := range cp.parsers {
		p.Close()
	}
	cp.parsers = make(map[string]*sitter.Parser)
	slog.Debug("parser resources released")
}
