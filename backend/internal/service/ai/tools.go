package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/cloudwego/eino-ext/components/tool/duckduckgo/v2"
	"github.com/cloudwego/eino-ext/components/tool/googlesearch"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
)

func InitToolsChain() []tool.BaseTool {
	var tools []tool.BaseTool

	if ws := InitWebSearch(); ws != nil {
		tools = append(tools, ws)
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
		httpClient: &http.Client{Timeout: 10 * time.Second},
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

func (w *webSearchTool) fetchURL(ctx context.Context, target string) (string, error) {
	if w.httpClient == nil {
		w.httpClient = &http.Client{Timeout: 10 * time.Second}
	}

	parsed, err := url.Parse(target)
	if err != nil {
		return "", fmt.Errorf("invalid url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("unsupported url scheme")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "UnichatGo-WebSearch/1.0")

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch url: %s", resp.Status)
	}

	const maxBodySize = 512 * 1024
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize))
	if err != nil {
		return "", err
	}

	return string(body), nil
}

func looksLikeURL(input string) bool {
	lower := strings.ToLower(input)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
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
