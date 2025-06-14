package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
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

type FormattedResponse struct {
	Type    string `json:"type"`    // CMD, EXP, or FULL
	Command string `json:"command"` // For CMD and FULL
	Content string `json:"content"` // For EXP and FULL
}

func callOllama(prompt string) (*FormattedResponse, error) {
	reqBody, _ := json.Marshal(ollamaRequest{
		Model:  "deepseek-r1",
		Prompt: prompt,
	})

	resp, err := http.Post("http://localhost:11434/api/generate", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("ollama request failed: %v", err)
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
			return nil, fmt.Errorf("failed to decode response: %v", err)
		}
		fullResponse.WriteString(result.Response)
	}

	// Parse the response to extract command/explanation
	response := &FormattedResponse{}
	rawResponse := fullResponse.String()

	if strings.HasPrefix(rawResponse, "[CMD]") {
		response.Type = "CMD"
		response.Command = strings.TrimSpace(strings.TrimPrefix(rawResponse, "[CMD]"))
	} else if strings.HasPrefix(rawResponse, "[EXP]") {
		response.Type = "EXP"
		response.Content = strings.TrimSpace(strings.TrimPrefix(rawResponse, "[EXP]"))
	} else if strings.HasPrefix(rawResponse, "[FULL]") {
		response.Type = "FULL"
		// Remove the [FULL] prefix
		content := strings.TrimPrefix(rawResponse, "[FULL]")

		// Split into command and explanation if both exist
		if strings.Contains(content, "## Explanation:") {
			parts := strings.SplitN(content, "## Explanation:", 2)
			// Clean up the command part
			cmdPart := strings.TrimPrefix(parts[0], "## Command:")
			response.Command = strings.TrimSpace(cmdPart)

			if len(parts) > 1 {
				response.Content = strings.TrimSpace(parts[1])
			}
		} else {
			// If no explicit split, treat as command
			response.Command = strings.TrimSpace(content)
		}
	} else {
		// Default to FULL type for unformatted responses
		response.Type = "FULL"
		response.Content = rawResponse
	}

	return response, nil
}

func main() {
	// Parse command line flags
	sseMode := flag.Bool("sse", true, "Run in SSE mode instead of stdio mode")
	flag.Parse()

	// Create MCP server with basic capabilities
	mcpServer := server.NewMCPServer(
		"K8s AI Agent (KAI)",
		"0.0.1",
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

		// Marshal the FormattedResponse to JSON
		responseJSON, err := json.Marshal(output)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to marshal response: %v", err)), nil
		}

		return mcp.NewToolResultText(string(responseJSON)), nil
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
