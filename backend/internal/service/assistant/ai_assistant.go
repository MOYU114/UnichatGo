package assistant

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"unichatgo/internal/config"
	"unichatgo/internal/models"
)

type assistantService struct {
	chatModel model.ToolCallingChatModel
	config    config.AssistantConfig
}

func NewAssistantService(token string) (*assistantService, error) {
	var chatModel model.ToolCallingChatModel
	var err error

	cfgPath := os.Getenv("UNICHATGO_CONFIG")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	switch cfg.Assistant.Provider {
	case "openai":
		chatModel, err = openai.NewChatModel(context.Background(), &openai.ChatModelConfig{
			BaseURL: cfg.Assistant.BaseURL,
			Model:   cfg.Assistant.Model,
			APIKey:  token})
		cfg.Assistant.APIToken = token
	default:
		log.Fatalf("unknown provider: %s", cfg.Assistant.Provider)
	}
	if err != nil {
		log.Fatalf("Init assistant failed: %v", err)
	}
	return &assistantService{
		chatModel: chatModel,
		config:    cfg.Assistant,
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
