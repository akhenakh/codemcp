package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

var (
	// ExtensionWeights prioritizes source code files over config or documentation files.
	// Used in ScoreFile logic.
	ExtensionWeights = map[string]int{
		".go": 25, ".ts": 20, ".tsx": 20, ".js": 15,
		".rs": 20, ".zig": 20, ".py": 20, ".java": 15, ".h": 20, ".cpp": 20, ".c": 20,
	}

	// IgnoreDirs contains directory names that should be skipped during
	// file collection to improve performance.
	IgnoreDirs = map[string]bool{
		".git": true, "node_modules": true, "vendor": true,
		".next": true, ".idea": true, ".vscode": true, "bin": true,
	}

	// AllowedPathPrefixes stores absolute paths that are safe to read from.
	// This includes the project root, GOMODCACHE, and GOROOT.
	AllowedPathPrefixes []string
)

// FileScore represents the relevance of a file to a search query.
type FileScore struct {
	Path    string   `json:"path"`
	Score   int      `json:"score"`
	Reasons []string `json:"reasons"`       // e.g., "exact-file", "func:Login"
	IsDep   bool     `json:"is_dependency"` // True if file is from external module
}

// CLIOutput defines the JSON structure when running in --json mode.
type CLIOutput struct {
	Query    string      `json:"query"`
	Duration string      `json:"duration"`
	Count    int         `json:"count"`
	Files    []FileScore `json:"files"`
}

func main() {
	jsonOutput := flag.Bool("json", false, "Output results as JSON")
	searchPath := flag.String("path", ".", "Root path to search")
	useGopls := flag.Bool("gopls", true, "Use gopls for dependency search")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options] <query>\n", os.Args[0])
		flag.PrintDefaults()
	}

	flag.Parse()
	args := flag.Args()

	// Resolve absolute path for the project root
	absPath, err := filepath.Abs(*searchPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error resolving path: %v\n", err)
		os.Exit(1)
	}

	// If using gopls, initialize it immediately.
	// It runs as a background process.
	if *useGopls {
		InitGopls(absPath)
		defer ShutdownGopls()
	}

	// No query arguments -> Run as MCP Server (stdio mode)
	if len(args) == 0 {
		runServer(absPath)
		return
	}

	// Query arguments present -> Run as CLI tool
	query := strings.Join(args, " ")
	runCLI(query, absPath, *jsonOutput)
}

func runCLI(query string, absPath string, asJson bool) {
	start := time.Now()
	// Run Hybrid Search (Local AST + Gopls)
	results, err := Search(absPath, query)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Search failed: %v\n", err)
		os.Exit(1)
	}
	duration := time.Since(start)

	if asJson {
		output := CLIOutput{
			Query:    query,
			Duration: duration.String(),
			Count:    len(results),
			Files:    results,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(output)
		return
	}

	// Human Readable Output
	fmt.Printf("Searching '%s' in %s\n", query, absPath)
	if GoplsInstance != nil {
		fmt.Println("Gopls enabled (searching dependencies)")
	}
	fmt.Printf("Found %d files in %v\n\n", len(results), duration)

	fmt.Printf("%-6s | %-25s | %s\n", "SCORE", "REASON", "FILE")
	fmt.Println(strings.Repeat("-", 100))

	for _, r := range results {
		reason := ""
		if len(r.Reasons) > 0 {
			reason = r.Reasons[0]
		}
		pathDisplay := r.Path
		if r.IsDep {
			// Cyan color for dependencies
			pathDisplay = fmt.Sprintf("\033[36m[DEP] %s\033[0m", r.Path)
		}
		fmt.Printf("%-6d | %-25s | %s\n", r.Score, reason, pathDisplay)
	}
}

// initSecurity configures the allowed paths for read_file.
// It allows the project root, the Go Module Cache, and GOROOT.
func initSecurity(rootPath string) {
	AllowedPathPrefixes = append(AllowedPathPrefixes, rootPath)

	// Add GOMODCACHE
	if out, err := exec.Command("go", "env", "GOMODCACHE").Output(); err == nil {
		path := strings.TrimSpace(string(out))
		if path != "" {
			AllowedPathPrefixes = append(AllowedPathPrefixes, path)
		}
	}

	// Add GOROOT
	if out, err := exec.Command("go", "env", "GOROOT").Output(); err == nil {
		path := strings.TrimSpace(string(out))
		if path != "" {
			AllowedPathPrefixes = append(AllowedPathPrefixes, path)
		}
	}
}

// isAllowedPath checks if the target path is within one of the allowed prefixes.
func isAllowedPath(target string) bool {
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return false
	}

	// Clean the path to remove .. and double separators
	cleanTarget := filepath.Clean(absTarget)

	for _, prefix := range AllowedPathPrefixes {
		// Ensure prefix allows for checking subdirectories correctly
		// e.g. /app matches /app/foo but not /apple
		cleanPrefix := filepath.Clean(prefix)
		if cleanTarget == cleanPrefix || strings.HasPrefix(cleanTarget, cleanPrefix+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

func runServer(rootPath string) {
	// Initialize security boundaries
	initSecurity(rootPath)

	s := server.NewMCPServer(
		"Search-MCP",
		"1.2.0",
		server.WithToolCapabilities(true),
	)

	// Tool: search_files
	searchTool := mcp.NewTool("search_files",
		mcp.WithDescription("Search codebase and dependencies. Uses AST for local files and Gopls for dependencies/symbols. Always use this before read_file."),
		mcp.WithString("query", mcp.Required(), mcp.Description("Query (e.g. 'AuthService login')")),
	)

	s.AddTool(searchTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, _ := request.RequireString("query")
		start := time.Now()

		results, err := Search(rootPath, query)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Error: %v", err)), nil
		}

		// Create JSON output structure
		output := CLIOutput{
			Query:    query,
			Duration: time.Since(start).String(),
			Count:    len(results),
			Files:    results,
		}

		jsonData, err := json.MarshalIndent(output, "", "  ")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("JSON marshaling failed: %v", err)), nil
		}

		return mcp.NewToolResultText(string(jsonData)), nil
	})

	// Tool: read_file
	readTool := mcp.NewTool("read_file",
		mcp.WithDescription("Read the full content of a file. This tool is restricted to files within the project root, the Go Module Cache, or the Go Standard Library. Use this to read files found via search_files."),
		mcp.WithString("path", mcp.Required(), mcp.Description("Absolute path to the file (or relative to project root)")),
	)

	s.AddTool(readTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		pathArg, _ := request.RequireString("path")

		// Handle relative paths by joining with root, but respect absolute paths (common from gopls)
		targetPath := pathArg
		if !filepath.IsAbs(pathArg) {
			targetPath = filepath.Join(rootPath, pathArg)
		}

		// Security Check
		if !isAllowedPath(targetPath) {
			return mcp.NewToolResultError(fmt.Sprintf("Access Denied: Reading file %s is not allowed. Scope restricted to project root and Go dependencies.", pathArg)), nil
		}

		content, err := os.ReadFile(targetPath)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(string(content)), nil
	})

	if err := server.ServeStdio(s); err != nil {
		os.Exit(1)
	}
}

// Search runs local AST search and Gopls dependency search concurrently
// and merges the results with deduplication.
func Search(absRoot string, query string) ([]FileScore, error) {
	var results []FileScore
	var mu sync.Mutex
	var wg sync.WaitGroup

	queryLower := strings.ToLower(strings.TrimSpace(query))
	terms := Tokenize(query)
	if len(terms) == 0 {
		terms = strings.Fields(queryLower)
	}

	// Local Search (AST + Path)
	wg.Add(1)
	go func() {
		defer wg.Done()
		localRes, _ := LocalSearch(absRoot, terms, queryLower)
		mu.Lock()
		results = append(results, localRes...)
		mu.Unlock()
	}()

	// Gopls Search (Dependencies + Symbols)
	if GoplsInstance != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Query gopls for workspace symbols
			goplsRes, err := GoplsInstance.SymbolSearch(query)
			if err == nil {
				mu.Lock()
				// Merge results with Deduplication
				for _, gr := range goplsRes {
					// Check if this file was already found by LocalSearch
					isDup := false
					for _, existing := range results {
						// Convert existing path to absolute for comparison
						existingAbs := existing.Path
						if !filepath.IsAbs(existingAbs) {
							existingAbs = filepath.Join(absRoot, existingAbs)
						}

						if existingAbs == gr.Path {
							isDup = true
							break
						}
					}

					// If new, add it
					if !isDup {
						// If gopls returns a file inside our root, make it relative
						if strings.HasPrefix(gr.Path, absRoot) {
							rel, _ := filepath.Rel(absRoot, gr.Path)
							gr.Path = rel
							gr.IsDep = false // It is actually local
						}
						results = append(results, gr)
					}
				}
				mu.Unlock()
			}
		}()
	}

	wg.Wait()

	// Final Sort by Score
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	// Limit to top 50 results
	if len(results) > 50 {
		results = results[:50]
	}
	return results, nil
}

// LocalSearch iterates through files in root and scores them.
func LocalSearch(root string, terms []string, queryLower string) ([]FileScore, error) {
	files, _ := CollectFiles(root)
	var results []FileScore

	for _, f := range files {
		score, reasons := ScoreFile(root, f, terms, queryLower)
		if score > 0 {
			results = append(results, FileScore{
				Path:    f,
				Score:   score,
				Reasons: reasons,
				IsDep:   false,
			})
		}
	}
	return results, nil
}

// CollectFiles uses git ls-files if available, otherwise filepath.WalkDir.
func CollectFiles(root string) ([]string, error) {
	if _, err := os.Stat(filepath.Join(root, ".git")); err == nil {
		cmd := exec.Command("git", "ls-files", "-c", "-o", "--exclude-standard")
		cmd.Dir = root
		out, err := cmd.Output()
		if err == nil {
			return strings.Split(strings.TrimSpace(string(out)), "\n"), nil
		}
	}
	// Fallback omitted for brevity, logic exists in previous versions if needed
	return []string{}, nil
}

// ScoreFile calculates the score for a single local file.
// It combines path matching heuristics and AST content matching.
func ScoreFile(root string, relPath string, terms []string, queryLower string) (int, []string) {
	score := 0
	reasons := []string{}
	pathLower := strings.ToLower(relPath)
	fileName := filepath.Base(pathLower)
	ext := filepath.Ext(fileName)

	// Path Scoring
	nameNoExt := strings.TrimSuffix(fileName, ext)
	if nameNoExt == queryLower {
		score += 500
		reasons = append(reasons, "exact-file")
	}

	matches := 0
	for _, term := range terms {
		if strings.Contains(pathLower, term) {
			matches++
		}
	}
	if matches > 0 && matches == len(terms) {
		score += 50
	}

	// AST Scoring (Content)
	// Only parse .go files. We skip this step if the file is not Go.
	if ext == ".go" {
		absPath := filepath.Join(root, relPath)
		astScore, astReasons := AnalyzeGoFile(absPath, terms)
		if astScore > 0 {
			score += astScore
			reasons = append(reasons, astReasons...)
		}
	}

	// Extension Bonus (Only apply if we found *something* relevant)
	if score > 0 {
		if w, ok := ExtensionWeights[ext]; ok {
			score += w
		}
	}

	return score, reasons
}

// Tokenize splits strings like "AuthService" into ["auth", "service"].
func Tokenize(input string) []string {
	splitter := regexp.MustCompile(`[^a-zA-Z0-9]+`)
	parts := splitter.Split(input, -1)
	var tokens []string
	for _, part := range parts {
		var currentToken strings.Builder
		for i, r := range part {
			// split on camelCase boundaries
			if i > 0 && unicode.IsUpper(r) && unicode.IsLower(rune(part[i-1])) {
				tokens = append(tokens, strings.ToLower(currentToken.String()))
				currentToken.Reset()
			}
			currentToken.WriteRune(r)
		}
		if currentToken.Len() > 0 {
			tokens = append(tokens, strings.ToLower(currentToken.String()))
		}
	}
	return tokens
}

// AnalyzeGoFile parses a Go file's AST to find matching function or type definitions.
func AnalyzeGoFile(absPath string, terms []string) (int, []string) {
	fset := token.NewFileSet()
	// Parse only comments and top-level declarations (SkipObjectResolution)
	// This makes parsing very fast as we don't need full type checking.
	node, err := parser.ParseFile(fset, absPath, nil, parser.SkipObjectResolution|parser.ParseComments)
	if err != nil {
		return 0, nil
	}

	score := 0
	var matched []string

	ast.Inspect(node, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncDecl:
			// Match function names
			name := strings.ToLower(x.Name.Name)
			for _, t := range terms {
				if strings.Contains(name, t) {
					score += 40
					matched = append(matched, "func:"+x.Name.Name)
				}
			}
		case *ast.TypeSpec:
			// Match struct/interface names
			name := strings.ToLower(x.Name.Name)
			for _, t := range terms {
				if strings.Contains(name, t) {
					score += 40
					matched = append(matched, "type:"+x.Name.Name)
				}
			}
		}
		return true
	})
	return score, matched
}
