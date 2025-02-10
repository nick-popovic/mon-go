package main

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

const defaultConnectionString = "mongodb://localhost:27017"
const defaultListLimit = 5

type model struct {
	client         *mongo.Client
	currentPath    []string // ["database", "collection", "document_id"]
	textInput      textinput.Model
	output         string
	err            error
	showAllResults bool
}

type mongoMsg struct {
	result string
	err    error
}

func initialModel(connectionString string) model {
	ti := textinput.New()
	ti.Placeholder = "Enter command..."
	ti.Focus()
	ti.Width = 50

	// Connect to MongoDB.  Handle errors gracefully.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(connectionString))
	if err != nil {
		// Instead of fatal, return an error state in the model.
		return model{textInput: ti, err: fmt.Errorf("failed to connect to MongoDB: %w", err)}
	}

	err = client.Ping(ctx, readpref.Primary())
	if err != nil {
		return model{textInput: ti, err: fmt.Errorf("failed to ping MongoDB: %w", err)}
	}

	return model{
		client:      client,
		currentPath: []string{},
		textInput:   ti,
		output:      "",
		err:         nil,
	}
}

func (m model) Init() tea.Cmd {
	return textinput.Blink
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyEnter:
			input := strings.TrimSpace(m.textInput.Value())
			m.textInput.SetValue("") // Clear input after processing
			return m.processCommand(input)

		case tea.KeyCtrlC, tea.KeyEsc:
			return m, tea.Quit
		}

	case mongoMsg:
		m.output = msg.result
		m.err = msg.err
		return m, nil // No further commands needed after a mongo operation

	case error:
		m.err = msg
		return m, nil
	}

	m.textInput, cmd = m.textInput.Update(msg) // Always update the text input
	return m, cmd
}

func (m model) View() string {
	var b strings.Builder
	b.WriteString("mon-go (")

	if len(m.currentPath) == 0 {
		b.WriteString("/")
	} else {
		b.WriteString(strings.Join(m.currentPath, "/"))
	}

	b.WriteString(") ")               // Just closing parenthesis and a space
	b.WriteString(m.textInput.View()) // this adds the > prompt at the end
	b.WriteString("\n\n")

	if m.err != nil {
		b.WriteString(fmt.Sprintf("Error: %v\n", m.err))
	} else {
		b.WriteString(m.output)
	}
	return b.String()
}

func (m *model) processCommand(input string) (tea.Model, tea.Cmd) {
	parts := strings.Fields(input) // Simple splitting, consider using a shell parser library
	if len(parts) == 0 {
		return m, nil // No command entered
	}

	command := parts[0]
	args := parts[1:]

	switch command {
	case "cd":
		if len(args) == 0 {
			m.currentPath = []string{} // Go to root
			m.output = ""
			return m, nil
		}
		return m, m.cd(args[0])
	case "ls":
		showAll := false
		if len(args) > 0 && args[0] == "-la" {
			showAll = true
		}
		return m, m.ls(showAll)
	default:
		m.err = fmt.Errorf("unknown command: %s", command)
		return m, nil
	}
}

func (m *model) cd(target string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		newPath := make([]string, len(m.currentPath))
		copy(newPath, m.currentPath)

		parts := strings.Split(target, "/") // Handle relative and absolute paths
		for _, part := range parts {
			if part == ".." {
				if len(newPath) > 0 {
					newPath = newPath[:len(newPath)-1] // Go up one level
				}
			} else if part != "." && part != "" { // Handle "." (current dir) and empty parts
				newPath = append(newPath, part)
			}
		}

		// Check validity of the new path with regex
		if len(newPath) > 0 {
			// Check if database exists
			dbNames, err := m.client.ListDatabaseNames(ctx, bson.M{})
			if err != nil {
				return mongoMsg{err: err}
			}
			dbRegex, err := regexp.Compile("^" + newPath[0] + "$")
			if err != nil {
				return mongoMsg{err: err}
			}
			dbExists := false
			for _, dbName := range dbNames {
				if dbRegex.MatchString(dbName) {
					dbExists = true
					break
				}
			}
			if !dbExists {
				return mongoMsg{err: fmt.Errorf("database '%s' does not exist", newPath[0])}
			}
		}
		if len(newPath) > 1 {
			// Check if collection exists
			collNames, err := m.client.Database(newPath[0]).ListCollectionNames(ctx, bson.M{})
			if err != nil {
				return mongoMsg{err: err}
			}
			collRegex, err := regexp.Compile("^" + newPath[1] + "$")
			if err != nil {
				return mongoMsg{err: err}
			}

			collExists := false
			for _, collName := range collNames {
				if collRegex.MatchString(collName) {
					collExists = true
					break
				}
			}
			if !collExists {
				return mongoMsg{err: fmt.Errorf("collection '%s' does not exist in database '%s'", newPath[1], newPath[0])}
			}
		}
		//if it reaches here, we can set the path without issue
		m.currentPath = newPath
		return mongoMsg{} // Empty result, just update the path.
	}
}

func (m *model) ls(showAll bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		var result strings.Builder
		limit := defaultListLimit
		if showAll {
			limit = -1 // Indicate no limit
		}

		switch len(m.currentPath) {
		case 0: // List databases
			dbNames, err := m.client.ListDatabaseNames(ctx, bson.M{})
			if err != nil {
				return mongoMsg{err: err}
			}
			for i, dbName := range dbNames {
				if limit != -1 && i >= limit {
					result.WriteString("... (results truncated)\n")
					break
				}
				result.WriteString(fmt.Sprintf("%s\n", dbName))
			}

		case 1: // List collections in the database
			dbName := m.currentPath[0]
			collNames, err := m.client.Database(dbName).ListCollectionNames(ctx, bson.M{})
			if err != nil {
				return mongoMsg{err: err}
			}
			for i, collName := range collNames {
				if limit != -1 && i >= limit {
					result.WriteString("... (results truncated)\n")
					break
				}
				result.WriteString(fmt.Sprintf("%s\n", collName))
			}
		case 2: // List documents in the collection
			dbName := m.currentPath[0]
			collName := m.currentPath[1]
			coll := m.client.Database(dbName).Collection(collName)

			var filter bson.M
			findOptions := options.Find()
			if limit != -1 {
				findOptions.SetLimit(int64(limit))
			}

			cur, err := coll.Find(ctx, filter, findOptions)
			if err != nil {
				return mongoMsg{err: err}
			}
			defer cur.Close(ctx)

			count := 0
			for cur.Next(ctx) {
				var doc bson.M
				if err := cur.Decode(&doc); err != nil {
					return mongoMsg{err: err}
				}
				result.WriteString(fmt.Sprintf("%v\n", doc))
				count++
			}

			if limit != -1 && count >= limit { // Check truncation *after* the loop
				result.WriteString("... (results truncated)\n")
			}

			if err := cur.Err(); err != nil {
				return mongoMsg{err: err}
			}

		case 3: // Show a single document
			dbName := m.currentPath[0]
			collName := m.currentPath[1]
			docID := m.currentPath[2]

			coll := m.client.Database(dbName).Collection(collName)

			objectID, err := primitive.ObjectIDFromHex(docID)
			if err != nil {
				return mongoMsg{err: fmt.Errorf("invalid document ID: %s", docID)}
			}
			var doc bson.M
			err = coll.FindOne(ctx, bson.M{"_id": objectID}).Decode(&doc)

			if err != nil {
				if err == mongo.ErrNoDocuments {
					return mongoMsg{err: fmt.Errorf("document with ID '%s' not found", docID)}
				}
				return mongoMsg{err: err}
			}
			result.WriteString(fmt.Sprintf("%v\n", doc))

		default:
			return mongoMsg{err: fmt.Errorf("invalid path depth")}
		}

		return mongoMsg{result: result.String()}
	}
}

func main() {
	connectionString := defaultConnectionString
	if len(os.Args) > 1 {
		connectionString = os.Args[1]
	}

	m := initialModel(connectionString)
	p := tea.NewProgram(&m, tea.WithAltScreen())

	if _, err := p.Run(); err != nil {
		fmt.Printf("Alas, there's been an error: %v", err)
		os.Exit(1)
	}
}
