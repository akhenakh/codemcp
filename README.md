# CodeMCP

An MCP (Model Context Protocol) server specialized for **Go codebases**. It provides structural code search capabilities for AI agents by combining:

1.  **Local AST Analysis**: Parses local Go files to find function, struct, and interface definitions.
2.  **Gopls Integration**: Queries the Go Language Server (`gopls`) to search across your **dependencies** and the Go standard library.
3.  **Hybrid Ranking**: Intelligently ranks results, prioritizing your business logic over library code.
4.  On non Go code it is performing fuzzy filename search.

## Installation

### Prerequisites
*   Go 1.22+
*   `gopls` (for dependency search):
    ```bash
    go install golang.org/x/tools/gopls@latest
    ```

### Build

```bash
go install github.com/akhenakh/codemcp@latest
# Be sure your GOPATH/bin is in your PATH
```

## Usage

### 1. CLI Mode (For Humans)

You can run `codemcp` directly in your terminal to find code.

```bash
# Search for "auth" in the current directory, it will look for auth in the code and in the dependencies (if it's a Go module)
codemcp "auth"
```

You can point it to a different path.
```bash
codemcp -path ../myrepo "authorize"

```

**Output Example:**
```text
🔎 Searching 'json unmarshal' in /home/user/myproject
🚀 Gopls enabled (searching dependencies)
⏱️  Found 5 files in 45ms

SCORE  | REASON                    | FILE
----------------------------------------------------------------------------------------------------
105    | func:Unmarshal            | encoding.go
80     | gopls:Unmarshal           | [DEP] /usr/lib/go/src/encoding/json/decode.go
65     | func:Decode               | [DEP] /usr/lib/go/src/encoding/json/stream.go
```

### 2. MCP Server Mode (For AI Agents)

When run without arguments, it starts the MCP server over stdio.
It should work with any agents.

#### Configuration for Mistral "Vibe Code"

Add the following to your agent configuration (e.g., `.vibe/config.toml`):

```toml
enabled_tools = ["bash", "grep", "read_file", "write_file", "todo", "search_replace", "codemcp_*"]

[[mcp_servers]]
name = "codemcp"
transport = "stdio"
command = "codemcp"
prompt = """
Use codemcp whenever the user asks to search for specific code patterns, definitions, or symbols. 
It is much smarter than grep because it understands Go structure and can search dependencies.
"""
```


## Tools Provided

The server exposes one primary tool:

*   **`search_files`**:
    *   **Arguments**: `query` (string), `path` (optional string).
    *   **Description**: "Search codebase and dependencies. Uses AST for local files and Gopls for dependencies/symbols. Always use this before read_file."

## How it Works

1.  **Tokenization**: Splits CamelCase queries (e.g., "UserLogin" -> "user", "login").
2.  **Local Scan**:
    *   Uses `git ls-files` for speed.
    *   Parses `.go` files using `go/parser` (AST).
    *   Boosts score if query matches a `func`, `type`, or `interface` name.
3.  **Dependency Scan**:
    *   Spawns `gopls` in the background.
    *   Sends LSP `workspace/symbol` requests.
    *   Filters out noise (test files, internal vendor folders).
    *   Applies a small penalty to dependencies so your local code ranks higher.
