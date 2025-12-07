package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

const (
	MCPEndpoint = "http://localhost:8080/sse"

	// ANSI color codes
	colorReset  = "\033[0m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorCyan   = "\033[36m"
	colorRed    = "\033[31m"
)

type FormattedResponse struct {
	Type    string `json:"type"`    // CMD, EXP, or FULL
	Command string `json:"command"` // For CMD and FULL
	Content string `json:"content"` // For EXP and FULL
}

type SessionContext struct {
	Namespace string
}

func formatWithPrePrompt(userInput string) string {
	prePrompt := `You are a Kubernetes and Cloud Native expert AI assistant. Follow these steps:

1. ANALYZE:
   - Identify the requested operation scope (pod, deployment, service, etc.)
   - Consider relevant Kubernetes concepts and CNCF tools
   - Check for potential security implications
   - Determine if this requires cluster-admin privileges

2. RECOMMEND:
   - Suggest appropriate kubectl or related commands
   - Follow Kubernetes best practices
   - Consider resource impact and safety
   - Include necessary flags and options
   - Provide proper namespace context if needed

3. FORMAT RESPONSE AS:
   - [CMD] format: Only return the command, example:
     kubectl get pods -n default

   - [EXP] format: Return markdown explanation, example:
     # Pod List Operation
     This command will list all pods in the default namespace.
     * Requires: view permissions
     * Impact: None (read-only operation)

   - [FULL] format: Return both command and explanation, example:
     ## Command:
     kubectl get pods -n default

     ## Explanation:
     This command lists all pods...
     
4. SAFETY CHECKS:
   - Highlight if command needs cluster-admin privileges
   - Warn about potential service disruptions
   - Suggest --dry-run=client when appropriate
   - Include resource quotas consideration
   - Mention any networking implications

5. COMMAND CONVENTIONS:
   - Always use long-form flags (--namespace instead of -n)
   - Include namespace when relevant
   - Add --context flag if multiple clusters
   - Quote string values containing special characters
   - Use proper resource abbreviations (po, svc, deploy)

To specify output format, prefix your query with:
[CMD] - for command only
[EXP] - for explanation only
[FULL] - for both command and explanation (default)

Example queries:
[CMD] scale frontend deployment to 3 replicas
[EXP] create a nodeport service for nginx
[FULL] delete all failed pods in kube-system namespace`

	if prePrompt == "" {
		return userInput
	}
	return fmt.Sprintf("%s\n\nUser Input: %s", prePrompt, userInput)
}

func main() {
	// Add a flag for default namespace
	defaultNamespace := ""
	if len(os.Args) > 1 {
		for i, arg := range os.Args {
			if (arg == "--namespace" || arg == "-n") && i+1 < len(os.Args) {
				defaultNamespace = os.Args[i+1]
			}
		}
	}

	mcpClient, err := client.NewSSEMCPClient(MCPEndpoint)
	if err != nil {
		log.Fatalf("failed to create MCP client: %v", err)
	}
	defer mcpClient.Close()

	if err := mcpClient.Start(context.Background()); err != nil {
		log.Fatalf("failed to start MCP client: %v", err)
	}

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
	fmt.Printf("Initialized %s %s\n", initResult.ServerInfo.Name, initResult.ServerInfo.Version)

	sessionCtx := &SessionContext{Namespace: defaultNamespace}

	reader := bufio.NewReader(os.Stdin)
	fmt.Println("\nEnter your prompts (type 'quit' to exit, ':namespace <ns>' to change namespace):")

	for {
		nsPrompt := ""
		if sessionCtx.Namespace != "" {
			nsPrompt = fmt.Sprintf(" [%s]", sessionCtx.Namespace)
		}
		fmt.Printf("\n%s> %s", colorCyan, nsPrompt)
		userInput, err := reader.ReadString('\n')
		fmt.Print(colorReset)
		if err != nil {
			log.Fatalf("Error reading input: %v", err)
		}

		userInput = strings.TrimSpace(userInput)
		if strings.EqualFold(userInput, "quit") {
			fmt.Println("Exiting.")
			break
		}

		// Namespace change command
		if strings.HasPrefix(userInput, ":namespace ") {
			ns := strings.TrimSpace(strings.TrimPrefix(userInput, ":namespace "))
			if ns != "" {
				sessionCtx.Namespace = ns
				fmt.Printf("%sNamespace set to '%s'%s\n", colorGreen, ns, colorReset)
			}
			continue
		}

		// Add namespace context to prompt if set
		formattedPrompt := userInput
		if sessionCtx.Namespace != "" {
			formattedPrompt = fmt.Sprintf("%s (namespace: %s)", userInput, sessionCtx.Namespace)
		}
		formattedPrompt = formatWithPrePrompt(formattedPrompt)

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

		// Replace the response handling section in the main loop
		if toolResultPtr != nil {
			if len(toolResultPtr.Content) == 0 {
				fmt.Println("No content in response.")
				continue
			}

			textContent, ok := toolResultPtr.Content[0].(mcp.TextContent)
			if !ok {
				fmt.Printf("%sUnsupported content type. Expected TextContent.%s\n", colorRed, colorReset)
				continue
			}

			var response FormattedResponse
			if err := json.Unmarshal([]byte(textContent.Text), &response); err != nil {
				fmt.Printf("\n%sError parsing response: %v%s\n", colorRed, err, colorReset)
				fmt.Printf("Raw response: %s\n", textContent.Text)
				continue
			}

			// Update session context if command contains --namespace
			if response.Command != "" && strings.Contains(response.Command, "--namespace") {
				parts := strings.Split(response.Command, "--namespace")
				if len(parts) > 1 {
					ns := strings.Fields(parts[1])[0]
					sessionCtx.Namespace = ns
					fmt.Printf("%s[Context] Namespace updated to: %s%s\n", colorCyan, ns, colorReset)
				}
			}

			// Print formatted response based on type
			switch response.Type {
			case "CMD":
				fmt.Printf("\n%sCommand:%s\n%s\n",
					colorGreen, colorReset, response.Command)
			case "EXP":
				fmt.Printf("\n%sExplanation:%s\n%s\n",
					colorYellow, colorReset, response.Content)
			case "FULL":
				if response.Command != "" {
					fmt.Printf("\n%sCommand:%s\n%s\n",
						colorGreen, colorReset, response.Command)
				}
				if response.Content != "" {
					fmt.Printf("\n%sExplanation:%s\n%s\n",
						colorYellow, colorReset, response.Content)
				}
			default:
				fmt.Printf("\n%sUnexpected response type: %s%s\n",
					colorCyan, response.Type, colorReset)
			}
		} else {
			fmt.Println("No response received.")
		}

	}
}
