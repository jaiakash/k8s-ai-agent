package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

const (
	MCPEndpoint = "http://localhost:8080/sse"
)

// Add new function to format the prompt with pre-prompt
func formatWithPrePrompt(userInput string) string {
	// Define your pre-prompt
	prePrompt := `You are a helpful AI assistant. Please provide clear and concise responses.
Format your responses using markdown when appropriate.
Keep your answers focused and relevant to the question.`

	if prePrompt == "" {
		return userInput
	}
	return fmt.Sprintf("%s\n\nUser Input: %s", prePrompt, userInput)
}

func main() {
	mcpClient, err := client.NewSSEMCPClient(MCPEndpoint)
	if err != nil {
		log.Fatalf("failed to create MCP client: %v", err)
	}
	defer mcpClient.Close()

	if err := mcpClient.Start(context.Background()); err != nil {
		log.Fatalf("failed to start MCP client: %v", err)
	}

	fmt.Printf("Initializing k8s mcp server...")
	initRequest := mcp.InitializeRequest{}
	initRequest.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initRequest.Params.ClientInfo = mcp.Implementation{
		Name:    "k8s mcp server",
		Version: "0.1.0",
	}
	initRequest.Params.Capabilities = mcp.ClientCapabilities{}

	ctx := context.Background()
	initResult, err := mcpClient.Initialize(ctx, initRequest)
	if err != nil {
		log.Fatalf("failed to initialize MCP client: %v", err)
	}
	fmt.Printf("Initialized with server: %s %s\n", initResult.ServerInfo.Name, initResult.ServerInfo.Version)

	reader := bufio.NewReader(os.Stdin)
	fmt.Println("\nEnter your prompts (type 'quit' to exit):")

	for {
		fmt.Print("\n> ")
		userInput, err := reader.ReadString('\n')
		if err != nil {
			log.Fatalf("Error reading input: %v", err)
		}

		userInput = strings.TrimSpace(userInput)
		if strings.EqualFold(userInput, "quit") {
			fmt.Println("Exiting.")
			break
		}

		// Format the prompt with pre-prompt
		formattedPrompt := formatWithPrePrompt(userInput)

		args := map[string]interface{}{
			"prompt": formattedPrompt,
		}

		req := mcp.CallToolRequest{}
		req.Params.Name = "ask_ollama"
		req.Params.Arguments = args

		toolResultPtr, err := mcpClient.CallTool(ctx, req)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			continue
		}

		if toolResultPtr != nil {
			fmt.Printf("\nResponse: %v\n", toolResultPtr.Content)
		} else {
			fmt.Println("No response received.")
		}

	}
}
