package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	"github.com/google/generative-ai-go/genai"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
	"google.golang.org/api/option"
)

// VectorStore registers a PocketBase extension that automatically generates and
// stores vector embeddings (via Google AI text-embedding-004) for records in the
// specified collections, and exposes a vector-search endpoint.
//
// It requires the GOOGLE_AI_API_KEY environment variable to be set.
// Example usage:
//
//	err := plugins.VectorStore(app, "articles")
func VectorStore(app *pocketbase.PocketBase, collections ...string) error {
	// Auto-load the sqlite-vec extension for all mattn/go-sqlite3 connections.
	// Must be called before any DB connections are opened (i.e. before app.Start()).
	sqlite_vec.Auto()

	// Lazily initialized on first use so that a missing GOOGLE_AI_API_KEY does
	// not prevent the server from starting.
	var (
		client     *genai.Client
		clientErr  error
		clientOnce sync.Once
	)
	getClient := func() (*genai.Client, error) {
		clientOnce.Do(func() {
			client, clientErr = createGoogleAiClient()
		})
		return client, clientErr
	}

	// After bootstrap completes, ensure the required collections and virtual tables exist.
	app.OnBootstrap().BindFunc(func(e *core.BootstrapEvent) error {
		// Call e.Next() first so the app finishes bootstrapping before we query the DB.
		if err := e.Next(); err != nil {
			return err
		}
		for _, target := range collections {
			collection, _ := app.FindCollectionByNameOrId(target)
			if collection == nil {
				if err := createVectorCollection(app, target); err != nil {
					return err
				}
			}
		}
		return nil
	})

	// Generate/update embeddings after a record is created.
	app.OnRecordAfterCreateSuccess().BindFunc(func(e *core.RecordEvent) error {
		tbl := e.Record.Collection().Name
		for _, target := range collections {
			if tbl == target {
				c, err := getClient()
				if err != nil {
					e.App.Logger().Warn("VectorStore: skipping embedding (no Google AI client): " + err.Error())
					break
				}
				if err := modelModify(e.App, target, c, e.Record); err != nil {
					return err
				}
			}
		}
		return e.Next()
	})

	// Regenerate embeddings after a record is updated.
	app.OnRecordAfterUpdateSuccess().BindFunc(func(e *core.RecordEvent) error {
		tbl := e.Record.Collection().Name
		for _, target := range collections {
			if tbl == target {
				c, err := getClient()
				if err != nil {
					e.App.Logger().Warn("VectorStore: skipping embedding (no Google AI client): " + err.Error())
					break
				}
				if err := modelModify(e.App, target, c, e.Record); err != nil {
					return err
				}
			}
		}
		return e.Next()
	})

	// Remove embeddings when a record is deleted.
	app.OnRecordAfterDeleteSuccess().BindFunc(func(e *core.RecordEvent) error {
		tbl := e.Record.Collection().Name
		for _, target := range collections {
			if tbl == target {
				if err := deleteEmbeddingsForRecord(e.App, target, e.Record); err != nil {
					return err
				}
			}
		}
		return e.Next()
	})

	// Register the vector-search API endpoint.
	app.OnServe().BindFunc(func(se *core.ServeEvent) error {
		se.Router.GET(
			"/api/collections/{collectionIdOrName}/records/vector-search",
			func(re *core.RequestEvent) error {
				target := re.Request.PathValue("collectionIdOrName")
				if _, err := re.App.FindCollectionByNameOrId(target); err != nil {
					re.App.Logger().Error(fmt.Sprint(err))
					return re.NotFoundError("Collection not found", err)
				}

				title := re.Request.URL.Query().Get("title")
				content := re.Request.URL.Query().Get("content")
				kNum := 5
				if k := re.Request.URL.Query().Get("k"); k != "" {
					if val, err := strconv.Atoi(k); err == nil {
						kNum = val
					}
				}

				if content == "" {
					return re.NoContent(http.StatusNoContent)
				}

				c, err := getClient()
				if err != nil {
					return re.InternalServerError("Vector search unavailable: Google AI API key is not configured", err)
				}
				vector, err := googleAiEmbedContent(c, genai.TaskTypeRetrievalQuery, title, genai.Text(content))
				if err != nil {
					return err
				}
				jsonVec, err := json.Marshal(vector)
				if err != nil {
					return err
				}

				stmt := "SELECT v.id, distance, v.content, v.created, v.updated "
				stmt += "FROM " + target + "_embeddings "
				stmt += "LEFT JOIN " + target + " v ON v.vector_id = " + target + "_embeddings.id "
				stmt += "WHERE embedding MATCH {:embedding} "
				stmt += "AND k = {:k};"

				results := []dbx.NullStringMap{}
				err = re.App.DB().
					NewQuery(stmt).
					Bind(dbx.Params{
						"embedding": string(jsonVec),
						"k":         kNum,
					}).
					All(&results)
				if err != nil {
					re.App.Logger().Error(fmt.Sprint(err))
					return re.InternalServerError("Vector search failed", err)
				}
				re.App.Logger().Info(fmt.Sprint(results))

				items := []map[string]any{}
				for _, result := range results {
					m := make(map[string]interface{})
					for key := range result {
						val := result[key]
						value, err := val.Value()
						if err != nil || !val.Valid {
							m[key] = nil
						} else {
							m[key] = value
						}
					}
					items = append(items, m)
				}

				// TODO: paging support
				return re.JSON(http.StatusOK, items)
			},
		)
		return se.Next()
	})

	return nil
}

// deleteEmbeddingsForRecord removes the embedding row associated with record from
// the <target>_embeddings virtual table.
func deleteEmbeddingsForRecord(app core.App, target string, record *core.Record) error {
	vectorId := record.GetInt("vector_id")
	if vectorId == 0 {
		return nil
	}
	stmt := "DELETE FROM " + target + "_embeddings WHERE id = {:id}"
	_, err := app.DB().NewQuery(stmt).Bind(dbx.Params{"id": vectorId}).Execute()
	if err != nil {
		// Silently ignore — the embedding row may not exist yet.
		return nil
	}
	return nil
}

// modelModify generates a new embedding for the record and updates the
// <target>_embeddings virtual table along with the record's vector_id column.
// The vector_id update is done via a raw SQL UPDATE to avoid re-triggering
// the OnRecordAfterUpdate hook (which would otherwise cause an infinite loop).
func modelModify(app core.App, target string, client *genai.Client, record *core.Record) error {
	title := record.GetString("title")
	content := record.GetString("content")

	result, err := googleAiEmbedContent(client, genai.TaskTypeRetrievalDocument, title, genai.Text(content))
	if err != nil {
		return err
	}

	vector := "[]"
	if jsonVec, err := json.Marshal(result); err == nil {
		vector = string(jsonVec)
	}

	// Remove stale embedding before inserting a fresh one.
	_ = deleteEmbeddingsForRecord(app, target, record)

	stmt := "INSERT INTO " + target + "_embeddings (embedding) VALUES ({:embedding});"
	res, err := app.DB().NewQuery(stmt).Bind(dbx.Params{"embedding": vector}).Execute()
	if err != nil {
		// Silently ignore embedding insert errors so the record save succeeds.
		return nil
	}
	vectorId, err := res.LastInsertId()
	if err != nil {
		return err
	}

	// Use a raw SQL UPDATE to set vector_id without going through app.Save(),
	// which would re-fire the OnRecordAfterUpdate hook (infinite loop).
	_, err = app.DB().NewQuery(
		"UPDATE "+target+" SET vector_id = {:vectorId} WHERE id = {:id}",
	).Bind(dbx.Params{
		"vectorId": vectorId,
		"id":       record.Id,
	}).Execute()
	return err
}

// createVectorCollection creates a new PocketBase collection with the required
// fields and a companion sqlite-vec virtual table for storing embeddings.
func createVectorCollection(app core.App, target string) error {
	collection := core.NewBaseCollection(target)
	collection.Fields.Add(&core.TextField{Name: "title"})
	collection.Fields.Add(&core.TextField{Name: "content", Required: true})
	collection.Fields.Add(&core.NumberField{Name: "vector_id"})
	collection.AddIndex("idx_"+target, true, "title, content, vector_id", "")

	if err := app.Save(collection); err != nil {
		return err
	}

	// Create the companion virtual table that stores float[768] embeddings.
	stmt := "CREATE VIRTUAL TABLE IF NOT EXISTS " + target + "_embeddings using vec0( "
	stmt += " id INTEGER PRIMARY KEY AUTOINCREMENT, "
	stmt += " embedding float[768] "
	stmt += ");"
	if _, err := app.DB().NewQuery(stmt).Execute(); err != nil {
		// Ignore — table may already exist (e.g. on a subsequent run).
		return nil
	}

	return nil
}

func createGoogleAiClient() (*genai.Client, error) {
	apiKey := os.Getenv("GOOGLE_AI_API_KEY")
	ctx := context.Background()
	client, err := genai.NewClient(ctx, option.WithAPIKey(apiKey))
	if err != nil {
		return nil, err
	}
	return client, nil
}

func googleAiEmbedContent(client *genai.Client, taskType genai.TaskType, title string, parts ...genai.Part) ([]float32, error) {
	ctx := context.Background()
	model := client.EmbeddingModel("text-embedding-004")
	model.TaskType = taskType
	res, err := model.EmbedContentWithTitle(ctx, title, parts...)
	if err != nil {
		return nil, err
	}
	return res.Embedding.Values, nil
}
