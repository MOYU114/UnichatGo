package ai

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"unichatgo/internal/config"
	"unichatgo/internal/models"

	"github.com/cloudwego/eino-ext/components/model/claude"
	"github.com/cloudwego/eino-ext/components/model/gemini"
	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/flow/agent/react"
	"github.com/cloudwego/eino/schema"
	"google.golang.org/genai"
)

type aiService struct {
	aiModel   model.ToolCallingChatModel
	config    *config.Config
	histories map[int64][]*models.Message
	todoTools []tool.BaseTool
	agent     *react.Agent
	mu        sync.RWMutex
}

func NewAiService(provider string, modelType string, token string) (*aiService, error) {
	var chatModel model.ToolCallingChatModel
	var err error
	cfgPath := os.Getenv("UNICHATGO_CONFIG")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	provCfg, ok := cfg.Providers[provider]
	if !ok {
		return nil, fmt.Errorf("provider %s not configured", provider)
	}
	if modelType == "" {
		modelType = provCfg.Model
	}
	todoTools := InitToolsChain()
	var reactAgent *react.Agent

	switch provider {
	case "openai":
		chatModel, err = openai.NewChatModel(context.Background(), &openai.ChatModelConfig{
			BaseURL: provCfg.BaseURL,
			Model:   modelType,
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
			Model:  modelType,
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
			Model:     modelType,
			BaseURL:   baseURLPtr,
			MaxTokens: 3000,
		})
	default:
		return nil, fmt.Errorf("invalid provider: %s", provider)
	}
	if err != nil {
		log.Fatalf("Start Ai Service failed: %v", err)
	}

	if len(todoTools) > 0 {
		reactAgent, err = react.NewAgent(context.Background(), &react.AgentConfig{
			ToolCallingModel: chatModel,
			ToolsConfig: compose.ToolsNodeConfig{
				Tools: todoTools,
			},
		})
		if err != nil {
			return nil, fmt.Errorf("init react agent: %w", err)
		}
	}

	return &aiService{
		aiModel:   chatModel,
		config:    cfg,
		histories: make(map[int64][]*models.Message),
		todoTools: todoTools,
		agent:     reactAgent,
	}, nil
}

// StreamChat Using stream chat to handle Ai output
func (s *aiService) StreamChat(ctx context.Context, message *models.Message, prevHistory []*models.Message, callback func(string) error) (*models.Message, error) {
	if message == nil {
		return nil, errors.New("message cannot be nil")
	}
	if message.SessionID == 0 {
		return nil, errors.New("session_id is required")
	}
	// prime cache with db history when provided
	if len(prevHistory) > 0 {
		s.loadHistory(message.SessionID, prevHistory)
	}
	// append latest user message
	s.appendHistory(message.SessionID, message)
	messagesEino := s.convertMessages(message.SessionID)

	var (
		streamReader *schema.StreamReader[*schema.Message]
		err          error
	)
	if s.agent != nil {
		streamReader, err = s.agent.Stream(ctx, messagesEino)
	} else {
		streamReader, err = s.aiModel.Stream(ctx, messagesEino)
	}
	if err != nil {
		return nil, fmt.Errorf("generate Ai stream failed: %w", err)
	}
	var fullContent string
	for {
		chunk, err := streamReader.Recv()
		if err != nil {
			// flow finished
			break
		}
		content := chunk.Content
		fullContent += content

		if callback != nil {
			if err := callback(fullContent); err != nil {
				return nil, err
			}
		}
	}
	response := &models.Message{
		UserID:    message.UserID,
		SessionID: message.SessionID,
		Role:      models.RoleAssistant,
		Content:   fullContent,
		CreatedAt: time.Now(),
	}
	s.appendHistory(message.SessionID, response)
	return response, nil
}

func (s *aiService) convertMessages(sessionID int64) []*schema.Message {
	s.mu.RLock()
	history := s.histories[sessionID]
	s.mu.RUnlock()

	messages := make([]*schema.Message, 0, len(history))
	for _, msg := range history {
		var role schema.RoleType
		switch msg.Role {
		case models.RoleUser:
			role = schema.User
		case models.RoleAssistant:
			role = schema.Assistant
		case models.RoleSystem:
			role = schema.System
		default:
			role = schema.User
		}

		messages = append(messages, &schema.Message{
			Role:    role,
			Content: msg.Content,
		})
	}
	return messages
}

func (s *aiService) loadHistory(sessionID int64, history []*models.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cloned := make([]*models.Message, 0, len(history))
	for _, msg := range history {
		if msg == nil {
			continue
		}
		copyMsg := *msg
		cloned = append(cloned, &copyMsg)
	}
	s.histories[sessionID] = cloned
}

func (s *aiService) appendHistory(sessionID int64, msg *models.Message) {
	if msg == nil {
		return
	}
	msgCopy := *msg
	s.mu.Lock()
	s.histories[sessionID] = append(s.histories[sessionID], &msgCopy)
	s.mu.Unlock()
}
