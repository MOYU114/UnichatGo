package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/cloudwego/eino-ext/components/document/loader/file"
	"github.com/cloudwego/eino-ext/components/tool/duckduckgo/v2"
	"github.com/cloudwego/eino-ext/components/tool/googlesearch"
	"github.com/cloudwego/eino/components/document"
	"github.com/cloudwego/eino/components/document/parser"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"

	"unichatgo/internal/models"
)

func InitToolsChain() []tool.BaseTool {
	var tools []tool.BaseTool

	if ws := InitWebSearch(); ws != nil {
		tools = append(tools, ws)
	}
	if fr := initTempFileReader(); fr != nil {
		tools = append(tools, fr)
	}
	return tools
}

func InitWebSearch() tool.InvokableTool {
	googleTool := InitGooglesearch()
	duckTool := InitDDGsearch()
	if googleTool == nil && duckTool == nil {
		log.Printf("web search tool disabled: no search providers available")
		return nil
	}

	ws := &webSearchTool{
		google:     googleTool,
		duck:       duckTool,
		httpClient: &http.Client{Timeout: WebSearchHTTPTimeout},
	}

	info := &schema.ToolInfo{
		Name: "web_search",
		Desc: "Search the web for information; " +
			"automatically fallbacks to another provider if needed;" +
			"can search URL if needed.",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"query": {
				Desc:     "Natural language query or URL to search",
				Type:     schema.String,
				Required: true,
			},
		}),
	}

	return utils.NewTool(info, ws.run)
}

type webSearchTool struct {
	google     tool.InvokableTool
	duck       tool.InvokableTool
	httpClient *http.Client
}

type webSearchParams struct {
	Query string `json:"query"`
}

func (w *webSearchTool) run(ctx context.Context, params *webSearchParams) (string, error) {
	if params == nil {
		return "", errors.New("missing search parameters")
	}
	query := strings.TrimSpace(params.Query)
	if query == "" {
		return "", errors.New("query must not be empty")
	}

	if looksLikeURL(query) {
		if content, err := w.fetchURL(ctx, query); err == nil {
			return content, nil
		} else {
			log.Printf("web url loader failed: %v", err)
		}
	}

	payloadBytes, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		return "", fmt.Errorf("marshal search params: %w", err)
	}
	payload := string(payloadBytes)

	if w.google != nil {
		if result, err := w.google.InvokableRun(ctx, payload); err == nil {
			return result, nil
		} else {
			log.Printf("google search failed: %v", err)
		}
	}

	if w.duck != nil {
		if result, err := w.duck.InvokableRun(ctx, payload); err == nil {
			return result, nil
		} else {
			log.Printf("duckduckgo search failed: %v", err)
		}
	}

	return "", errors.New("no search provider succeeded")
}

// temp file reader tool
type tempFileReader struct {
	loader *file.FileLoader
}

var tempFileReaderLimiter = newToolRateLimiter(TempFileRateLimit, TempFileRateWindow)

type tempFileReaderParams struct {
	FileID     int64 `json:"file_id"`
	ChunkIndex int   `json:"chunk_index,omitempty"`
	ChunkSize  int   `json:"chunk_size,omitempty"`
}

func initTempFileReader() tool.InvokableTool {
	parserExt, err := parser.NewExtParser(context.Background(), &parser.ExtParserConfig{
		FallbackParser: parser.TextParser{},
	})
	if err != nil {
		log.Printf("temp file reader disabled: %v", err)
		return nil
	}
	loader, err := file.NewFileLoader(context.Background(), &file.FileLoaderConfig{
		UseNameAsID: true,
		Parser:      parserExt,
	})
	if err != nil {
		log.Printf("temp file reader disabled: %v", err)
		return nil
	}
	reader := &tempFileReader{
		loader: loader,
	}
	info := &schema.ToolInfo{
		Name: "temp_file_reader",
		Desc: "Read user-uploaded documents in small chunks. Provide the file_id (and optional chunk_index / chunk_size) to fetch a specific segment; limit 3 calls per minute per session.",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"file_id": {
				Desc:     "ID of the file to read, provided in the system instructions.",
				Type:     schema.Integer,
				Required: true,
			},
			"chunk_index": {
				Desc:     "Zero-based chunk index to read, default 0.",
				Type:     schema.Integer,
				Required: false,
			},
			"chunk_size": {
				Desc:     "Number of characters per chunk (max 4000, default 2000).",
				Type:     schema.Integer,
				Required: false,
			},
		}),
	}
	return utils.NewTool(info, reader.run)
}

func (t *tempFileReader) run(ctx context.Context, params *tempFileReaderParams) (string, error) {
	if params == nil || params.FileID <= 0 {
		return "", errors.New("file_id is required")
	}
	files := TempFilesFromContext(ctx)
	if len(files) == 0 {
		return "", errors.New("no temp files available for this session")
	}
	var target *models.TempFile
	for _, f := range files {
		if f != nil && f.ID == params.FileID {
			target = f
			break
		}
	}
	if target == nil {
		return "", errors.New("file not found in current session")
	}
	userID, sessionID, ok := ToolSessionFromContext(ctx)
	key := fmt.Sprintf("file:%d", params.FileID)
	if ok {
		key = fmt.Sprintf("user:%d:session:%d", userID, sessionID)
	}
	if !tempFileReaderLimiter.Allow(key) {
		return "", errors.New("temp file reader rate limit exceeded, please retry in a minute")
	}

	docs, err := t.loader.Load(ctx, document.Source{URI: target.StoredPath})
	if err != nil {
		return "", fmt.Errorf("load file: %w", err)
	}
	var builder strings.Builder
	for _, doc := range docs {
		content := strings.TrimSpace(doc.Content)
		if content == "" {
			continue
		}
		builder.WriteString(content)
		builder.WriteString("\n\n")
	}
	text := strings.TrimSpace(builder.String())
	if text == "" {
		return "", errors.New("file has no readable text content")
	}
	chunkSize := params.ChunkSize
	if chunkSize <= 0 || chunkSize > TempFileChunkSizeMax {
		chunkSize = TempFileChunkSizeDefault
	}
	if chunkSize < TempFileChunkSizeMin {
		chunkSize = TempFileChunkSizeMin
	}
	chunkIndex := params.ChunkIndex
	if chunkIndex < 0 {
		chunkIndex = 0
	}
	runes := []rune(text)
	totalChunks := (len(runes) + chunkSize - 1) / chunkSize
	if totalChunks == 0 {
		return fmt.Sprintf("File: %s has no readable text content.", target.FileName), nil
	}
	if chunkIndex >= totalChunks {
		chunkIndex = totalChunks - 1
	}
	start := chunkIndex * chunkSize
	end := start + chunkSize
	if end > len(runes) {
		end = len(runes)
	}
	segment := string(runes[start:end])
	return fmt.Sprintf("File: %s\nChunk %d/%d\n\n%s", target.FileName, chunkIndex+1, totalChunks, segment), nil
}

// InitDDGsearch Init DDG Search
func InitDDGsearch() tool.InvokableTool {
	duckConfig := &duckduckgo.Config{
		ToolName:   "web_search_ddg",
		ToolDesc:   "DuckDuckGo Search Tool (no token required)",
		MaxResults: 3,
		Region:     duckduckgo.RegionWT,
		Timeout:    10 * time.Second,
	}
	duckTool, err := duckduckgo.NewTextSearchTool(context.Background(), duckConfig)
	if err != nil {
		log.Fatalf("NewTextSearchTool of duckduckgo failed, err=%v", err)
	}
	return duckTool
}

// InitGooglesearch Init Google Search
func InitGooglesearch() tool.InvokableTool {
	googleAPIKey := os.Getenv("GOOGLE_API_KEY")
	googleSearchEngineID := os.Getenv("GOOGLE_SEARCH_ENGINE_ID")
	if googleAPIKey == "" || googleSearchEngineID == "" {
		log.Printf("google search tool disabled: missing GOOGLE_API_KEY or GOOGLE_SEARCH_ENGINE_ID")
		return nil
	}
	googleTool, err := googlesearch.NewTool(context.Background(), &googlesearch.Config{
		ToolName:       "web_search_google",
		ToolDesc:       "Google Search Tool",
		APIKey:         googleAPIKey,
		SearchEngineID: googleSearchEngineID,
		Lang:           "en",
		Num:            5,
	})
	if err != nil {
		log.Fatal(err)
	}
	return googleTool
}
