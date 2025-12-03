package ai

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/cloudwego/eino-ext/components/tool/duckduckgo/v2"
	"github.com/cloudwego/eino-ext/components/tool/googlesearch"
	"github.com/cloudwego/eino/components/tool"
)

func InitToolsChain() []tool.BaseTool {
	var tools []tool.BaseTool

	searchToolDDG := InitDDGsearch()
	if searchToolDDG != nil {
		tools = append(tools, searchToolDDG)
	}

	searchToolGoogle := InitGooglesearch()
	if searchToolGoogle != nil {
		tools = append(tools, searchToolGoogle)
	}
	return tools
}

// InitDDGsearch Init DDG Search
func InitDDGsearch() tool.InvokableTool {
	duckConfig := &duckduckgo.Config{
		ToolName:   "web_search_DDK",
		ToolDesc:   "Duckduckgo Search Tool, Don't need api token.",
		MaxResults: 3, // Limit to return 20 results
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
