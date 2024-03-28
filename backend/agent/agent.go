package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/invopop/jsonschema"
	"github.com/jackc/pgx/v5/pgtype"
	openai "github.com/sashabaranov/go-openai"
	"github.com/semanser/ai-coder/assets"
	"github.com/semanser/ai-coder/config"
	"github.com/semanser/ai-coder/database"
	"github.com/semanser/ai-coder/services"
	"github.com/semanser/ai-coder/templates"
)

type Message string

type InputArgs struct {
	Query string
}

type TerminalArgs struct {
	Input string
	Message
}

type BrowserAction string

const (
	Read BrowserAction = "read"
	Url  BrowserAction = "url"
)

type BrowserArgs struct {
	Url    string
	Action BrowserAction
	Message
}

type CodeAction string

const (
	ReadFile   CodeAction = "read_file"
	UpdateFile CodeAction = "update_file"
)

type CodeArgs struct {
	Action  CodeAction
	Content string
	Path    string
	Message
}

type AskArgs struct {
	Message
}

type DoneArgs struct {
	Message
}

type AgentPrompt struct {
	Tasks       []database.Task
	DockerImage string
}

func NextTask(args AgentPrompt) *database.Task {
	log.Println("Getting next task")

	prompt, err := templates.Render(assets.PromptTemplates, "prompts/agent.tmpl", args)

	// TODO In case of lots of tasks, we should try to get a summary using gpt-3.5
	if len(prompt) > 30000 {
		log.Println("Prompt too long, asking user")
		return defaultAskTask("My prompt is too long and I can't process it")
	}

	if err != nil {
		log.Println("Failed to render prompt, asking user, %w", err)
		return defaultAskTask("There was an error getting the next task")
	}

	tools := []openai.Tool{
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "terminal",
				Description: "Calls a terminal command",
				Parameters:  jsonschema.Reflect(&TerminalArgs{}).Definitions["TerminalArgs"],
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "browser",
				Description: "Opens a browser to look for additional information",
				Parameters:  jsonschema.Reflect(&BrowserArgs{}).Definitions["BrowserArgs"],
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "code",
				Description: "Modifies or reads code files",
				Parameters:  jsonschema.Reflect(&CodeArgs{}).Definitions["CodeArgs"],
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "ask",
				Description: "Sends a question to the user for additional information",
				Parameters:  jsonschema.Reflect(&AskArgs{}).Definitions["AskArgs"],
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "done",
				Description: "Mark the whole task as done. Should be called at the very end when everything is completed",
				Parameters:  jsonschema.Reflect(&DoneArgs{}).Definitions["DoneArgs"],
			},
		},
	}

	var messages []openai.ChatCompletionMessage

	messages = append(messages, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleSystem,
		Content: prompt,
	})

	for _, task := range args.Tasks {
		if task.Type.String == "input" {
			messages = append(messages, openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleUser,
				Content: string(task.Args),
			})
		}

		if task.Type.String == "terminal" || task.Type.String == "code" || task.Type.String == "browser" || task.Type.String == "done" || (task.Type.String == "ask" && task.ToolCallID.String != "") {
			messages = append(messages, openai.ChatCompletionMessage{
				Role: openai.ChatMessageRoleAssistant,
				ToolCalls: []openai.ToolCall{
					{
						ID: task.ToolCallID.String,
						Function: openai.FunctionCall{
							Name:      task.Type.String,
							Arguments: string(task.Args),
						},
						Type: openai.ToolTypeFunction,
					},
				},
			})

			messages = append(messages, openai.ChatCompletionMessage{
				Role:       openai.ChatMessageRoleTool,
				ToolCallID: task.ToolCallID.String,
				Content:    task.Results.String,
			})
		}

		// This Ask was generated by the agent itself in case of some error (not the OpenAI)
		if task.Type.String == "ask" && task.ToolCallID.String == "" {
			messages = append(messages, openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleAssistant,
				Content: task.Message.String,
			})
		}
	}

	req := openai.ChatCompletionRequest{
		Temperature: 0.0,
		Model:       config.Config.OpenAIModel,
		Messages:    messages,
		ResponseFormat: &openai.ChatCompletionResponseFormat{
			Type: openai.ChatCompletionResponseFormatTypeJSONObject,
		},
		TopP:  0.2,
		Tools: tools,
		N:     1,
	}

	resp, err := services.OpenAIclient.CreateChatCompletion(context.Background(), req)
	if err != nil {
		log.Printf("Failed to get response from OpenAI %v", err)
		return defaultAskTask("There was an error getting the next task")
	}

	choices := resp.Choices

	if len(choices) == 0 {
		log.Println("No choices found, asking user")
		return defaultAskTask("Looks like I couldn't find a task to run")
	}

	toolCalls := choices[0].Message.ToolCalls

	if len(toolCalls) == 0 {
		log.Println("No tool calls found, asking user")
		return defaultAskTask("I couln't find a task to run")
	}

	tool := toolCalls[0]

	if tool.Function.Name == "" {
		log.Println("No tool found, asking user")
		return defaultAskTask("The next task is empty, I don't know what to do next")
	}

	task := database.Task{
		Type: database.StringToPgText(tool.Function.Name),
	}

	switch tool.Function.Name {
	case "terminal":
		params, err := extractArgs(tool.Function.Arguments, &TerminalArgs{})
		if err != nil {
			log.Printf("Failed to extract terminal args, asking user: %v", err)
			return defaultAskTask("There was an error running the terminal command")
		}
		args, err := json.Marshal(params)
		if err != nil {
			log.Printf("Failed to marshal terminal args, asking user: %v", err)
			return defaultAskTask("There was an error running the terminal command")
		}
		task.Args = args

		// Sometimes the model returns an empty string for the message
		msg := string(params.Message)
		if msg == "" {
			msg = params.Input
		}

		task.Message = database.StringToPgText(msg)
		task.Status = database.StringToPgText("in_progress")

	case "browser":
		params, err := extractArgs(tool.Function.Arguments, &BrowserArgs{})
		if err != nil {
			log.Printf("Failed to extract browser args, asking user: %v", err)
			return defaultAskTask("There was an error opening the browser")
		}
		args, err := json.Marshal(params)
		if err != nil {
			log.Printf("Failed to marshal browser args, asking user: %v", err)
			return defaultAskTask("There was an error opening the browser")
		}
		task.Args = args
		task.Message = pgtype.Text{
			String: string(params.Message),
			Valid:  true,
		}
	case "code":
		params, err := extractArgs(tool.Function.Arguments, &CodeArgs{})
		if err != nil {
			log.Printf("Failed to extract code args, asking user: %v", err)
			return defaultAskTask("There was an error reading or updating the file")
		}
		args, err := json.Marshal(params)
		if err != nil {
			log.Printf("Failed to marshal code args, asking user: %v", err)
			return defaultAskTask("There was an error reading or updating the file")
		}
		task.Args = args
		task.Message = pgtype.Text{
			String: string(params.Message),
			Valid:  true,
		}
	case "ask":
		params, err := extractArgs(tool.Function.Arguments, &AskArgs{})
		if err != nil {
			log.Printf("Failed to extract ask args, asking user: %v", err)
			return defaultAskTask("There was an error asking the user for additional information")
		}
		args, err := json.Marshal(params)
		if err != nil {
			log.Printf("Failed to marshal ask args, asking user: %v", err)
			return defaultAskTask("There was an error asking the user for additional information")
		}
		task.Args = args
		task.Message = pgtype.Text{
			String: string(params.Message),
			Valid:  true,
		}
	case "done":
		params, err := extractArgs(tool.Function.Arguments, &DoneArgs{})
		if err != nil {
			log.Printf("Failed to extract done args, asking user: %v", err)
			return defaultAskTask("There was an error marking the task as done")
		}
		args, err := json.Marshal(params)
		if err != nil {
			return defaultAskTask("There was an error marking the task as done")
		}
		task.Args = args
		task.Message = pgtype.Text{
			String: string(params.Message),
			Valid:  true,
		}
	}

	task.ToolCallID = pgtype.Text{
		String: tool.ID,
		Valid:  true,
	}

	return &task
}

func defaultAskTask(message string) *database.Task {
	task := database.Task{
		Type: database.StringToPgText("ask"),
	}

	task.Args = []byte("{}")
	task.Message = pgtype.Text{
		String: fmt.Sprintf("%s. What should I do next?", message),
		Valid:  true,
	}

	return &task
}

func extractArgs[T any](openAIargs string, args *T) (*T, error) {
	err := json.Unmarshal([]byte(openAIargs), args)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal args: %v", err)
	}
	return args, nil
}
