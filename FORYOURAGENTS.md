
## Available Tools

Use **CodeMCP** for structural code search in Go projects. It combines AST analysis with gopls to find functions, structs, and interfaces across your codebase and dependencies.

## When to Use

- **Go Code Search**: Finding function/struct/interface definitions
- **Dependency Exploration**: Understanding external library code
- **Standard Library Reference**: Looking up Go stdlib implementations
- **Build Debugging**: Investigating type/interface issues

### codemcp_search_files

Search codebase and dependencies.

**Parameters:**
- `query` (string, required): Search query (e.g., "AuthService login")

**Returns:** JSON with matched files, scores, and reasons.

**Example:**
```json
{
  "query": "json unmarshal",
  "duration": "123.456ms",
  "count": 2,
  "files": [
    {
      "path": "auth/service.go",
      "score": 105,
      "reasons": ["func:UnmarshalJSON"],
      "is_dependency": false
    },
    {
      "path": "/usr/lib/go/src/encoding/json/decode.go",
      "score": 50,
      "reasons": ["gopls:Unmarshal"],
      "is_dependency": true
    }
  ]
}
```

### codemcp_read_file

Read file contents. Works with both local files and Go dependencies.

**Parameters:**
- `path` (string, required): Absolute or relative file path

**Example:**
```json
{
  "path": "/usr/lib/go/src/encoding/json/decode.go"
}
```
## Workflow
1. **Search first**: Always use `codemcp_search_files` before reading files
2. **Check scores**: Higher scores = more relevant results
3. **Read dependencies**: Use absolute paths from search results to read stdlib/dependency code
4. **Prefer over grep**: Use codemcp for Go code instead of grep (it understands AST structure)
