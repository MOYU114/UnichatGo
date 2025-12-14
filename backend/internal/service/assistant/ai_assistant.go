package assistant

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/cloudwego/eino-ext/components/model/claude"
	"github.com/cloudwego/eino-ext/components/model/gemini"
	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"google.golang.org/genai"

	"unichatgo/internal/config"
	"unichatgo/internal/models"
)

type assistantService struct {
	chatModel model.ToolCallingChatModel
}

func NewAssistantService(provider, modelName, token string) (*assistantService, error) {
	var chatModel model.ToolCallingChatModel
	var err error

	cfgPath := os.Getenv("UNICHATGO_CONFIG")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	provCfg, ok := cfg.Providers[provider]
	if !ok {
		log.Fatalf("unknown provider: %s", provider)
	}
	if modelName == "" {
		modelName = provCfg.Model
	}

	switch provider {
	case "openai":
		chatModel, err = openai.NewChatModel(context.Background(), &openai.ChatModelConfig{
			BaseURL: provCfg.BaseURL,
			Model:   modelName,
			APIKey:  token})
	case "gemini":
		client, err := genai.NewClient(context.Background(), &genai.ClientConfig{
			APIKey: token,
		})
		if err != nil {
			log.Fatalf("NewClient of gemini failed, err=%v", err)
		}
		chatModel, err = gemini.NewChatModel(context.Background(), &gemini.Config{
			Client: client,
			Model:  modelName,
			ThinkingConfig: &genai.ThinkingConfig{
				IncludeThoughts: true,
				ThinkingBudget:  nil,
			},
		})
	case "claude":
		var baseURLPtr *string
		if provCfg.BaseURL != "" {
			baseURLPtr = &provCfg.BaseURL
		}
		chatModel, err = claude.NewChatModel(context.Background(), &claude.Config{
			APIKey:    token,
			Model:     modelName,
			BaseURL:   baseURLPtr,
			MaxTokens: 3000,
		})
	default:
		log.Fatalf("unknown provider: %s", provider)
	}
	if err != nil {
		log.Fatalf("Init assistant failed: %v", err)
	}
	return &assistantService{
		chatModel: chatModel,
	}, nil
}

func (as *assistantService) GenerateTitle(ctx context.Context, messages []*models.Message) (string, error) {
	if messages == nil || len(messages) == 0 {
		return "New Conversation", nil
	}
	defaultPrompt := "You are a conversation title generator. " +
		"Based on the dialogue between the user and the AI, generate a concise and accurate title for the conversation. " +
		"The title should be within 10 characters and summarize the main topic of the conversation. " +
		"Output only the title; do not include any additional content."
	// Get conversation context
	conversationText := ""
	for _, msg := range messages {
		if msg.Role == models.RoleUser {
			conversationText += fmt.Sprintf("User: %s\n", msg.Content)
		} else if msg.Role == models.RoleAssistant {
			conversationText += fmt.Sprintf("Assistant: %s\n", msg.Content)
		}
	}

	userPrompt := fmt.Sprintf("Please generate a clean title using following conversation messages:\n\n%s", conversationText)
	// Build messages array
	schemaMessages := []*schema.Message{
		{
			Role:    schema.System,
			Content: defaultPrompt,
		},
		{
			Role:    schema.User,
			Content: userPrompt,
		},
	}
	resp, err := as.chatModel.Generate(ctx, schemaMessages)
	if err != nil {
		return "", fmt.Errorf("generate title failed: %w", err)

	}
	title := resp.Content
	if len(title) == 0 {
		return "New Conversation", nil
	}
	return title, nil
}

func (as *assistantService) SummarizeFile(ctx context.Context, content []*models.Message) (string, error) {
	if content == nil || len(content) == 0 {
		return "", nil
	}

	systemPrompt := "You are a helpful assistant that summarizes user provided documents. " +
		"Produce a concise summary highlighting the key points and important details. " +
		"Limit the summary to 6 sentences."

	conversationText := ""
	for _, cnt := range content {
		if cnt.Role == models.RoleUser {
			conversationText += fmt.Sprintf("User: %s\n", cnt.Content)
		} else if cnt.Role == models.RoleAssistant {
			conversationText += fmt.Sprintf("Assistant: %s\n", cnt.Content)
		}
	}
	userPrompt := fmt.Sprintf("Document Content:\n%s\n", conversationText)
	schemaMessages := []*schema.Message{
		{
			Role:    schema.System,
			Content: systemPrompt,
		},
		{
			Role:    schema.User,
			Content: userPrompt,
		},
	}
	resp, err := as.chatModel.Generate(ctx, schemaMessages)
	if err != nil {
		return "", fmt.Errorf("summarize file failed: %w", err)
	}
	return resp.Content, nil
}
