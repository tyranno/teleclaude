//go:build windows

package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Design Ref: §1 (self-spawned stdio MCP server), §2 (tool table), §4 (mcpscreen.go).
//
// RunMCPScreen runs the embedded "screen" MCP server over stdio (blocking).
// teleclaude re-invokes itself with the hidden "__mcp-screen" subcommand to
// start this server; the claude worker connects to it via --mcp-config.
//
// This is the Windows implementation. Tools start with list_windows and
// focus_window (more added in later tasks: snapshot/screenshot/click/...).
func RunMCPScreen() error {
	s := server.NewMCPServer(
		"screen",
		"0.1.0",
		server.WithToolCapabilities(true),
	)

	// list_windows — visible top-level windows as "TITLE | hwnd=0x..".
	s.AddTool(
		mcp.NewTool("list_windows",
			mcp.WithDescription("List visible top-level windows. Returns one window per line as 'TITLE | hwnd=0x..'."),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			wins := enumWindows()
			if len(wins) == 0 {
				return mcp.NewToolResultText("(no visible windows)"), nil
			}
			var b strings.Builder
			for _, w := range wins {
				fmt.Fprintf(&b, "%s | hwnd=0x%x\n", w.Title, w.HWND)
			}
			return mcp.NewToolResultText(strings.TrimRight(b.String(), "\n")), nil
		},
	)

	// focus_window — bring a window to the foreground by title or hwnd.
	s.AddTool(
		mcp.NewTool("focus_window",
			mcp.WithDescription("Bring a window to the foreground. Accepts a window title substring (case-insensitive) or an hwnd like '0x1234'."),
			mcp.WithString("window",
				mcp.Description("Window title substring or hwnd (e.g. '0x1234')."),
				mcp.Required(),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			target, err := req.RequireString("window")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'window'"), nil
			}
			if err := focusWindow(target); err != nil {
				return mcp.NewToolResultErrorFromErr("focus_window failed", err), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf("ok: focused %q", target)), nil
		},
	)

	return server.ServeStdio(s)
}
