# Web search in this gateway project

When a task requires web search, call `mcp__gateway-search__search_web` with a specific query. This tool uses the search engine saved on the gateway Config page. Do not call Claude Code's built-in `WebSearch` for this project; it does not use the gateway search setting. If the MCP tool is unavailable, say that the gateway must be running. Do not treat a built-in `WebSearch` response of "Did 0 searches" as evidence that the gateway search returned no results.
