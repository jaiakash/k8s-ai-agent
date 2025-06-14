package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type ollamaRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

type ollamaResponse struct {
	Response string `json:"response"`
}

func callOllama(prompt string) (string, error) {
	reqBody, _ := json.Marshal(ollamaRequest{
		Model:  "deepseek-r1", // Change to your local model name
		Prompt: prompt,
	})

	resp, err := http.Post("http://localhost:11434/api/generate", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var fullResponse strings.Builder
	dec := json.NewDecoder(resp.Body)
	for dec.More() {
		var result ollamaResponse
		if err := dec.Decode(&result); err != nil {
			if err == io.EOF {
				break
			}
			return "", err
		}
		fullResponse.WriteString(result.Response)
	}

	return fullResponse.String(), nil
}

func main() {
	// Parse command line flags
	sseMode := flag.Bool("sse", true, "Run in SSE mode instead of stdio mode")
	flag.Parse()

	// Create MCP server with basic capabilities
	mcpServer := server.NewMCPServer(
		"Ollama LLM Demo",
		"1.0.0",
		server.WithRecovery(),
	)

	ollamaTool := mcp.NewTool("ask_ollama",
		mcp.WithDescription("Send a prompt to local Ollama LLM"),
		mcp.WithString("prompt",
			mcp.Required(),
			mcp.Description("Prompt to send to Ollama"),
		),
	)

	mcpServer.AddTool(ollamaTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		prompt, err := request.RequireString("prompt")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		output, err := callOllama(prompt)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(output), nil
	})

	// Run server in appropriate mode based on the sseMode flag
	if *sseMode {
		// Create and start SSE server for real-time communication
		sseServer := server.NewSSEServer(mcpServer,
			server.WithBaseURL("http://localhost:8080"))
		log.Printf("Starting SSE server on port :8080")
		if err := sseServer.Start(":8080"); err != nil {
			log.Fatalf("Server error: %v", err)
		}
	} else {
		// Run as stdio server for direct command-line interaction
		if err := server.ServeStdio(mcpServer); err != nil {
			log.Fatalf("Server error: %v", err)
		}
	}
}
