# Query Agent
Text-to-SQL with LLM. 

The service starts at http://localhost:8080/ after the following commands:
```bash
git clone https://github.com/andrewbrdk/queryagent
cd ./queryagent
npm install typescript
go get queryagent
go generate
go build
OPENROUTER_API_KEY=your-api-key OPENROUTER_MODEL=your/model QUERYAGENT_CONTEXT_PATH=./context_examples/ QUERYAGENT_LOG_FILE=logs/q.log ./queryagent
```

Docker compose starts the service, Postgres, and Pgweb.
```bash
OPENROUTER_API_KEY=your-api-key OPENROUTER_MODEL=your/model docker compose up --build

# Run once to populate the database
psql postgres://pguser:password123@localhost:5432/queryagent -f context_examples/example.sql
```
http://localhost:8080/ - Query Agent  
localhost:5432 - Postgres  
http://localhost:8081/ - Pgweb  
  
Env. variables
```bash
OPENROUTER_API_KEY               # (required) API key for OpenRouter
OPENROUTER_MODEL                 # (required) Model to use
QUERYAGENT_EXEC_DB               # Postgres connection string for SQL execution
QUERYAGENT_CONTEXT_PATH          # Path to a file or directory with SQL context examples
QUERYAGENT_LOG_FILE              # Path to a log file for LLM queries
QUERYAGENT_PASSWORD              # Password for the web UI
QUERYAGENT_PORT                  # HTTP port (default: `8080`)
QUERYAGENT_SLACK_SIGNING_SECRET  # Slack signing secret for the `/slack/slash` endpoint  
```