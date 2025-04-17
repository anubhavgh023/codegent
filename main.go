package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/google/generative-ai-go/genai"
	"github.com/invopop/jsonschema"
	"github.com/joho/godotenv"
	"google.golang.org/api/option"
)

func main() {
	// Load .env file
	if err := godotenv.Load(); err != nil {
		log.Fatal("Error loading .env file")
	}

	ctx := context.Background()

	// Initialize gemini client
	client, err := genai.NewClient(ctx, option.WithAPIKey(os.Getenv("GEMINI_API_KEY")))
	if err != nil {
		log.Fatal("ERROR not able to establish connection:", err)
	}
	defer client.Close()

	scanner := bufio.NewScanner(os.Stdin)
	getUserMessage := func() (string, bool) {
		if !scanner.Scan() {
			return "", false
		}
		return scanner.Text(), true
	}

	tools := []ToolDefinition{
		ReadFileDefinition,  // Tool-1 => reads file
		ListFilesDefinition, // Tool-2 => lists file
		EditFileDefinition,  // Tool-3 => edits files
	}
	agent := NewAgent(client, getUserMessage, tools)
	if err := agent.Run(ctx); err != nil {
		log.Println("ERROR in running: ", err.Error())
	}
}

// Agent struct 
type Agent struct {
	client         *genai.Client
	getUserMessage func() (string, bool)
	tools          []ToolDefinition
}

func NewAgent(
	client *genai.Client,
	getUserMessage func() (string, bool),
	tools []ToolDefinition,
) *Agent {
	return &Agent{
		client:         client,
		getUserMessage: getUserMessage,
		tools:          tools,
	}
}

func (a *Agent) Run(ctx context.Context) error {
	// Select model
	model := a.client.GenerativeModel("gemini-2.0-flash")

	// Model settings
	model.SetMaxOutputTokens(4096)

	// Tools for gemini
	geminiTools := make([]*genai.Tool, 0, len(a.tools))
	for _, tool := range a.tools {
		geminiTools = append(geminiTools, &genai.Tool{
			FunctionDeclarations: []*genai.FunctionDeclaration{{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  &tool.InputSchema,
			}},
		})
	}

	// Set tools on the model
	model.Tools = geminiTools

	// Start a chat session
	session := model.StartChat()

	fmt.Println("=== Chat with Gemini (use 'ctrl-c' to quit) ===")

	for {
		// Prompt for user input
		fmt.Print("\u001b[94mYou\u001b[0m: ")
		userInput, ok := a.getUserMessage()
		if !ok {
			break
		}

		// Send the user message and get response
		resp, err := a.runInference(ctx, session, userInput)
		if err != nil {
			log.Println("ERROR running inference:", err.Error())
			return err
		}

		// Process response parts
		toolCalls := []genai.FunctionCall{}
		for _, part := range resp.Candidates[0].Content.Parts {
			switch v := part.(type) {
			case genai.Text:
				fmt.Printf("\u001b[93mGemini\u001b[0m: %v\n", v)
			case genai.FunctionCall:
				toolCalls = append(toolCalls, v)
			}
		}

		// If there are tool calls, execute them and send results back to the model
		if len(toolCalls) > 0 {
			toolParts := make([]genai.Part, 0, len(toolCalls))
			for _, call := range toolCalls {
				result := a.executeTool(call.Name, call.Args)
				toolParts = append(toolParts, genai.FunctionResponse{
					Name:     call.Name,
					Response: result,
				})
			}

			// Send tool responses back to the model
			resp, err = session.SendMessage(ctx, toolParts...)
			if err != nil {
				log.Println("ERROR sending tool response:", err.Error())
				return err
			}

			// Print the model's response after tool execution
			for _, part := range resp.Candidates[0].Content.Parts {
				if text, ok := part.(genai.Text); ok {
					fmt.Printf("\u001b[93mGemini\u001b[0m: %v\n", text)
				}
			}
		}

		// Continue the loop to get new user input
	}
	return nil
}

func (a *Agent) executeTool(name string, input map[string]interface{}) map[string]interface{} {
	var toolDef ToolDefinition
	var found bool
	for _, tool := range a.tools {
		if tool.Name == name {
			toolDef = tool
			found = true
			break
		}
	}
	if !found {
		return map[string]interface{}{"error": "tool not found"}
	}

	inputJSON, _ := json.Marshal(input)
	fmt.Printf("\u001b[92mtool\u001b[0m: %s(%s)\n", name, inputJSON)
	response, err := toolDef.Function(inputJSON)
	if err != nil {
		return map[string]interface{}{"error": err.Error()}
	}
	return map[string]interface{}{"result": response}
}

func (a *Agent) runInference(
	ctx context.Context,
	session *genai.ChatSession,
	userInput string,
) (*genai.GenerateContentResponse, error) {
	// Send the user message to the model
	response, err := session.SendMessage(ctx, genai.Text(userInput))
	if err != nil {
		return nil, fmt.Errorf("error sending message: %v", err)
	}
	return response, nil
}

// Tool Definition
type ToolDefinition struct {
	Name        string       `json:"name"`
	Description string       `json:"description"`
	InputSchema genai.Schema `json:"input_schema"`
	Function    func(input json.RawMessage) (string, error)
}

// ReadFile Tool
var ReadFileDefinition = ToolDefinition{
	Name:        "read_file",
	Description: "Read the contents of a given relative file path. Use this when you want to see what's inside a file. Do not use this with directory names.",
	InputSchema: GenerateSchema[ReadFileInput](),
	Function:    ReadFile,
}

type ReadFileInput struct {
	Path string `json:"path" jsonschema_description:"The relative path of a file in the working directory."`
}

// List File Tool
var ListFilesDefinition = ToolDefinition{
	Name:        "list_files",
	Description: "List files and directories at a given path. If no path is provided, lists files in the current directory.",
	InputSchema: GenerateSchema[ListFilesInput](),
	Function:    ListFiles,
}

type ListFilesInput struct {
	Path string `json:"path,omitempty" jsonschema_description:"Optional relative path to list files from. Defaults to current directory if not provided."`
}

// Edit Tool
var EditFileDefinition = ToolDefinition{
	Name: "edit_file",
	Description: `Make edits to a text file.

Replaces 'old_str' with 'new_str' in the given file. 'old_str' and 'new_str' MUST be different from each other.

If the file specified with path doesn't exist, it will be created with new_str as its contents when old_str is empty.
`,
	InputSchema: GenerateSchema[EditFileInput](),
	Function:    EditFile,
}

type EditFileInput struct {
	Path   string `json:"path" jsonschema_description:"The path to the file"`
	OldStr string `json:"old_str" jsonschema_description:"Text to search for - must match exactly. Use empty string to create a new file."`
	NewStr string `json:"new_str" jsonschema_description:"Text to replace old_str with, or contents for a new file if old_str is empty"`
}

func GenerateSchema[T any]() genai.Schema {
	reflector := jsonschema.Reflector{
		AllowAdditionalProperties: false,
		DoNotReference:            true,
		RequiredFromJSONSchemaTags: true,
	}
	var v T

	schema := reflector.Reflect(v)

	// Convert jsonschema properties to genai.Schema properties
	properties := make(map[string]*genai.Schema)
	required := make([]string, 0)

	// Extract required fields from the schema
	if schema.Required != nil {
		required = schema.Required
	}

	// Only include properties that are actually defined
	for pair := schema.Properties.Newest(); pair != nil; pair = pair.Next() {
		key := pair.Key
		jsSchema := pair.Value

		// Map JSON schema types to genai.Schema types
		var schemaType genai.Type
		switch jsSchema.Type {
		case "string":
			schemaType = genai.TypeString
		case "number":
			schemaType = genai.TypeNumber
		case "integer":
			schemaType = genai.TypeInteger
		case "boolean":
			schemaType = genai.TypeBoolean
		case "array":
			schemaType = genai.TypeArray
		case "object":
			schemaType = genai.TypeObject
		default:
			schemaType = genai.TypeString // Default to string if unknown
		}

		properties[key] = &genai.Schema{
			Type:        schemaType,
			Description: jsSchema.Description,
		}
	}

	// Verify each required property exists in properties map
	filteredRequired := make([]string, 0, len(required))
	for _, req := range required {
		if _, exists := properties[req]; exists {
			filteredRequired = append(filteredRequired, req)
		}
	}

	// Create a genai.Schema for the object
	return genai.Schema{
		Type:       genai.TypeObject,
		Properties: properties,
		Required:   filteredRequired,
	}
}

func ReadFile(input json.RawMessage) (string, error) {
	readFileInput := ReadFileInput{}
	err := json.Unmarshal(input, &readFileInput)
	if err != nil {
		return "", err
	}

	content, err := os.ReadFile(readFileInput.Path)
	if err != nil {
		return "", err
	}
	return string(content), nil
}

func ListFiles(input json.RawMessage) (string, error) {
	listFilesInput := ListFilesInput{}
	err := json.Unmarshal(input, &listFilesInput)
	if err != nil {
		return "", fmt.Errorf("failed to parse input: %w", err)
	}

	dir := "."
	if listFilesInput.Path != "" {
		dir = listFilesInput.Path
	}

	files := make([]string, 0)
	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}

		if relPath != "." {
			if d.IsDir() {
				files = append(files, relPath+"/")
			} else {
				files = append(files, relPath)
			}
		}
		return nil
	})

	if err != nil {
		return "", err
	}

	result, err := json.Marshal(files)
	if err != nil {
		return "", err
	}

	return string(result), nil
}

func EditFile(input json.RawMessage) (string, error) {
	var editFileInput EditFileInput
	if err := json.Unmarshal(input, &editFileInput); err != nil {
		// Handle the case where we might have incomplete inputs
		var partialInput map[string]interface{}
		if jsonErr := json.Unmarshal(input, &partialInput); jsonErr == nil {
			// Create default values for missing fields
			if path, ok := partialInput["path"]; !ok || path == "" {
				// Set default path if missing
				editFileInput.Path = "./fizzbuzz.js"
			} else {
				editFileInput.Path = path.(string)
			}
			
			if oldStr, ok := partialInput["old_str"]; !ok {
				// If old_str is missing, set it to empty to create a new file
				editFileInput.OldStr = ""
			} else if oldStr != nil {
				editFileInput.OldStr = oldStr.(string)
			}
			
			if newStr, ok := partialInput["new_str"]; ok && newStr != nil {
				editFileInput.NewStr = newStr.(string)
			}
		} else {
			return "", fmt.Errorf("invalid input format: %w", err)
		}
	}

	// Validate that we have the necessary fields
	if editFileInput.Path == "" {
		editFileInput.Path = "./failed.txt" // Default path if not specified
	}
	
	if editFileInput.OldStr == editFileInput.NewStr && editFileInput.OldStr != "" {
		return "", fmt.Errorf("old_str and new_str must be different")
	}

	// Handle file creation or modification
	fileExists := true
	content, err := os.ReadFile(editFileInput.Path)
	if err != nil {
		if os.IsNotExist(err) {
			fileExists = false
			// For new files, we'll accept an empty old_str
			if editFileInput.OldStr != "" {
				return "", fmt.Errorf("file does not exist and old_str is not empty")
			}
		} else {
			return "", err
		}
	}

	// Either create a new file or modify an existing one
	if !fileExists {
		return createNewFile(editFileInput.Path, editFileInput.NewStr)
	} else {
		oldContent := string(content)
		newContent := strings.Replace(oldContent, editFileInput.OldStr, editFileInput.NewStr, -1)

		if oldContent == newContent && editFileInput.OldStr != "" {
			return "", fmt.Errorf("old_str not found in file")
		}

		if err := os.WriteFile(editFileInput.Path, []byte(newContent), 0644); err != nil {
			return "", err
		}

		return fmt.Sprintf("File %s updated successfully", editFileInput.Path), nil
	}
}

func createNewFile(filePath, content string) (string, error) {
	dir := path.Dir(filePath)
	if dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return "", fmt.Errorf("failed to create directory: %w", err)
		}
	}

	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		return "", fmt.Errorf("failed to create file: %w", err)
	}

	return fmt.Sprintf("Successfully created file %s", filePath), nil
}
