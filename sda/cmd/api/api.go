package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/casbin/casbin/v2"
	"github.com/casbin/casbin/v2/model"
	"github.com/gin-gonic/gin"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/neicnordic/crypt4gh/keys"
	"github.com/neicnordic/sensitive-data-archive/internal/broker"
	"github.com/neicnordic/sensitive-data-archive/internal/config"
	"github.com/neicnordic/sensitive-data-archive/internal/database"
	"github.com/neicnordic/sensitive-data-archive/internal/jsonadapter"
	"github.com/neicnordic/sensitive-data-archive/internal/schema"
	"github.com/neicnordic/sensitive-data-archive/internal/userauth"
	log "github.com/sirupsen/logrus"
)

type dataset struct {
	AccessionIDs []string `json:"accession_ids"`
	DatasetID    string   `json:"dataset_id"`
	User         string   `json:"user"`
}

var (
	Conf *config.Config
	err  error
	auth *userauth.ValidateFromToken
)

func main() {
	Conf, err = config.NewConfig("api")
	if err != nil {
		log.Fatal(err)
	}
	Conf.API.MQ, err = broker.NewMQ(Conf.Broker)
	if err != nil {
		log.Fatal(err)
	}
	Conf.API.DB, err = database.NewSDAdb(Conf.Database)
	if err != nil {
		log.Fatal(err)
	}

	if err := setupJwtAuth(); err != nil {
		log.Fatalf("error when setting up JWT auth, reason %s", err.Error())
	}

	sigc := make(chan os.Signal, 5)
	signal.Notify(sigc, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	go func() {
		<-sigc
		shutdown()
		os.Exit(0)
	}()

	srv := setup(Conf)
	if Conf.API.ServerCert != "" && Conf.API.ServerKey != "" {
		log.Infof("Starting web server at https://%s:%d", Conf.API.Host, Conf.API.Port)
		if err := srv.ListenAndServeTLS(Conf.API.ServerCert, Conf.API.ServerKey); err != nil {
			shutdown()
			log.Fatalln(err)
		}
	} else {
		log.Infof("Starting web server at http://%s:%d", Conf.API.Host, Conf.API.Port)
		if err := srv.ListenAndServe(); err != nil {
			shutdown()
			log.Fatalln(err)
		}
	}

}

func setup(config *config.Config) *http.Server {
	model, _ := model.NewModelFromString(jsonadapter.Model)
	e, err := casbin.NewEnforcer(model, jsonadapter.NewAdapter(&Conf.API.RBACpolicy))
	if err != nil {
		shutdown()
		log.Fatalf("error when setting up RBAC enforcer, reason %s", err.Error())
	}

	r := gin.Default()
	r.GET("/ready", readinessResponse)
	r.GET("/files", rbac(e), getFiles)
	// admin endpoints below here
	r.POST("/c4gh-keys/add", rbac(e), addC4ghHash)                      // Adds a key hash to the database
	r.GET("/c4gh-keys/list", rbac(e), listC4ghHashes)                   // Lists key hashes in the database
	r.POST("/c4gh-keys/deprecate/*keyHash", rbac(e), deprecateC4ghHash) // Deprecate a given key hash
	// submission endpoints below here
	r.POST("/file/ingest", rbac(e), ingestFile)                  // start ingestion of a file
	r.POST("/file/accession", rbac(e), setAccession)             // assign accession ID to a file
	r.POST("/dataset/create", rbac(e), createDataset)            // maps a set of files to a dataset
	r.POST("/dataset/release/*dataset", rbac(e), releaseDataset) // Releases a dataset to be accessible
	r.GET("/users", rbac(e), listActiveUsers)                    // Lists all users
	r.GET("/users/:username/files", rbac(e), listUserFiles)      // Lists all unmapped files for a user
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}

	srv := &http.Server{
		Addr:              config.API.Host + ":" + fmt.Sprint(config.API.Port),
		Handler:           r,
		TLSConfig:         cfg,
		TLSNextProto:      make(map[string]func(*http.Server, *tls.Conn, http.Handler)),
		ReadHeaderTimeout: 20 * time.Second,
		ReadTimeout:       5 * time.Minute,
		WriteTimeout:      2 * time.Minute,
	}

	return srv
}

func setupJwtAuth() error {
	auth = userauth.NewValidateFromToken(jwk.NewSet())
	if Conf.Server.Jwtpubkeyurl != "" {
		if err := auth.FetchJwtPubKeyURL(Conf.Server.Jwtpubkeyurl); err != nil {
			return err
		}
	}
	if Conf.Server.Jwtpubkeypath != "" {
		if err := auth.ReadJwtPubKeyPath(Conf.Server.Jwtpubkeypath); err != nil {
			return err
		}
	}

	return nil
}

func shutdown() {
	defer Conf.API.MQ.Channel.Close()
	defer Conf.API.MQ.Connection.Close()
	defer Conf.API.DB.Close()
}

func readinessResponse(c *gin.Context) {
	statusCode := http.StatusOK

	if Conf.API.MQ.Connection.IsClosed() {
		statusCode = http.StatusServiceUnavailable
		newConn, err := broker.NewMQ(Conf.Broker)
		if err != nil {
			log.Errorf("failed to reconnect to MQ, reason: %v", err)
		} else {
			Conf.API.MQ = newConn
		}
	}

	if Conf.API.MQ.Channel.IsClosed() {
		statusCode = http.StatusServiceUnavailable
		Conf.API.MQ.Connection.Close()
		newConn, err := broker.NewMQ(Conf.Broker)
		if err != nil {
			log.Errorf("failed to reconnect to MQ, reason: %v", err)
		} else {
			Conf.API.MQ = newConn
		}
	}

	if DBRes := checkDB(Conf.API.DB, 5*time.Millisecond); DBRes != nil {
		log.Debugf("DB connection error :%v", DBRes)
		Conf.API.DB.Reconnect()
		statusCode = http.StatusServiceUnavailable
	}

	c.JSON(statusCode, "")
}

func checkDB(database *database.SDAdb, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if database.DB == nil {
		return fmt.Errorf("database is nil")
	}

	return database.DB.PingContext(ctx)
}

func rbac(e *casbin.Enforcer) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, err := auth.Authenticate(c.Request)
		if err != nil {
			log.Debugln("bad token")
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": err.Error()})

			return
		}

		ok, err := e.Enforce(token.Subject(), c.Request.URL.String(), c.Request.Method)
		if err != nil {
			log.Debugf("rbac enforcement failed, reason: %s\n", err.Error())
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})

			return
		}
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "not authorized"})

			return
		}
		log.Debugln("authoriozed")
	}
}

// getFiles returns the files from the database for a specific user
func getFiles(c *gin.Context) {
	c.Writer.Header().Set("Content-Type", "application/json")
	// Get user ID to extract all files
	token, err := auth.Authenticate(c.Request)
	if err != nil {
		// something went wrong with user token
		c.JSON(401, err.Error())

		return
	}

	files, err := Conf.API.DB.GetUserFiles(token.Subject())
	if err != nil {
		// something went wrong with querying or parsing rows
		c.JSON(502, err.Error())

		return
	}

	// Return response
	c.JSON(200, files)
}

func ingestFile(c *gin.Context) {
	var ingest schema.IngestionTrigger
	if err := c.BindJSON(&ingest); err != nil {
		c.AbortWithStatusJSON(
			http.StatusBadRequest,
			gin.H{
				"error":  "json decoding : " + err.Error(),
				"status": http.StatusBadRequest,
			},
		)

		return
	}

	ingest.Type = "ingest"
	marshaledMsg, _ := json.Marshal(&ingest)
	if err := schema.ValidateJSON(fmt.Sprintf("%s/ingestion-trigger.json", Conf.Broker.SchemasPath), marshaledMsg); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, err.Error())

		return
	}

	corrID, err := Conf.API.DB.GetCorrID(ingest.User, ingest.FilePath)
	if err != nil {
		switch {
		case corrID == "":
			c.AbortWithStatusJSON(http.StatusBadRequest, err.Error())
		default:
			c.AbortWithStatusJSON(http.StatusInternalServerError, err.Error())
		}

		return
	}

	err = Conf.API.MQ.SendMessage(corrID, Conf.Broker.Exchange, "ingest", marshaledMsg)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, err.Error())

		return
	}

	c.Status(http.StatusOK)
}

func setAccession(c *gin.Context) {
	var accession schema.IngestionAccession
	if err := c.BindJSON(&accession); err != nil {
		c.AbortWithStatusJSON(
			http.StatusBadRequest,
			gin.H{
				"error":  "json decoding : " + err.Error(),
				"status": http.StatusBadRequest,
			},
		)

		return
	}

	corrID, err := Conf.API.DB.GetCorrID(accession.User, accession.FilePath)
	if err != nil {
		switch {
		case corrID == "":
			c.AbortWithStatusJSON(http.StatusBadRequest, err.Error())
		default:
			c.AbortWithStatusJSON(http.StatusInternalServerError, err.Error())
		}

		return
	}

	fileInfo, err := Conf.API.DB.GetFileInfo(corrID)
	if err != nil {
		log.Debugln(err.Error())
		c.AbortWithStatusJSON(http.StatusInternalServerError, err.Error())

		return
	}

	accession.DecryptedChecksums = []schema.Checksums{{Type: "sha256", Value: fileInfo.DecryptedChecksum}}
	accession.Type = "accession"
	marshaledMsg, _ := json.Marshal(&accession)
	if err := schema.ValidateJSON(fmt.Sprintf("%s/ingestion-accession.json", Conf.Broker.SchemasPath), marshaledMsg); err != nil {
		log.Debugln(err.Error())
		c.AbortWithStatusJSON(http.StatusBadRequest, err.Error())

		return
	}

	err = Conf.API.MQ.SendMessage(corrID, Conf.Broker.Exchange, "accession", marshaledMsg)
	if err != nil {
		log.Debugln(err.Error())
		c.AbortWithStatusJSON(http.StatusInternalServerError, err.Error())

		return
	}

	c.Status(http.StatusOK)
}

func createDataset(c *gin.Context) {
	var dataset dataset
	if err := c.BindJSON(&dataset); err != nil {
		c.AbortWithStatusJSON(
			http.StatusBadRequest,
			gin.H{
				"error":  "json decoding : " + err.Error(),
				"status": http.StatusBadRequest,
			},
		)

		return
	}

	if len(dataset.AccessionIDs) == 0 {
		c.AbortWithStatusJSON(http.StatusBadRequest, "at least one accessionID is reqired")

		return
	}

	for _, stableID := range dataset.AccessionIDs {
		inboxPath, err := Conf.API.DB.GetInboxPath(stableID)
		if err != nil {
			switch {
			case err.Error() == "sql: no rows in result set":
				log.Debugln(err.Error())
				c.AbortWithStatusJSON(http.StatusBadRequest, fmt.Sprintf("accession ID not found: %s", stableID))

				return
			default:
				log.Debugln(err.Error())
				c.AbortWithStatusJSON(http.StatusInternalServerError, err.Error())

				return
			}
		}

		_, err = Conf.API.DB.GetCorrID(dataset.User, inboxPath)
		if err != nil {
			switch {
			case err.Error() == "sql: no rows in result set":
				log.Debugln(err.Error())
				c.AbortWithStatusJSON(http.StatusBadRequest, "accession ID owned by other user")

				return
			default:
				log.Debugln(err.Error())
				c.AbortWithStatusJSON(http.StatusInternalServerError, err.Error())

				return
			}
		}
	}

	mapping := schema.DatasetMapping{
		Type:         "mapping",
		AccessionIDs: dataset.AccessionIDs,
		DatasetID:    dataset.DatasetID,
	}
	marshaledMsg, _ := json.Marshal(&mapping)
	if err := schema.ValidateJSON(fmt.Sprintf("%s/dataset-mapping.json", Conf.Broker.SchemasPath), marshaledMsg); err != nil {
		log.Debugln(err.Error())
		c.AbortWithStatusJSON(http.StatusBadRequest, err.Error())

		return
	}

	err = Conf.API.MQ.SendMessage("", Conf.Broker.Exchange, "mappings", marshaledMsg)
	if err != nil {
		log.Debugln(err.Error())
		c.AbortWithStatusJSON(http.StatusInternalServerError, err.Error())

		return
	}

	c.Status(http.StatusOK)
}

func releaseDataset(c *gin.Context) {
	datasetID := strings.TrimPrefix(c.Param("dataset"), "/")
	ok, err := Conf.API.DB.CheckIfDatasetExists(datasetID)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, err.Error())

		return
	}
	if !ok {
		c.AbortWithStatusJSON(http.StatusNotFound, "dataset not found")

		return
	}

	status, err := Conf.API.DB.GetDatasetStatus(datasetID)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, err.Error())

		return
	}
	if status != "registered" {
		c.AbortWithStatusJSON(http.StatusBadRequest, fmt.Sprintf("dataset already %s", status))

		return
	}

	datasetMsg := schema.DatasetRelease{
		Type:      "release",
		DatasetID: datasetID,
	}
	marshaledMsg, _ := json.Marshal(&datasetMsg)
	if err := schema.ValidateJSON(fmt.Sprintf("%s/dataset-release.json", Conf.Broker.SchemasPath), marshaledMsg); err != nil {
		log.Debugln(err.Error())
		c.AbortWithStatusJSON(http.StatusBadRequest, err.Error())

		return
	}

	err = Conf.API.MQ.SendMessage("", Conf.Broker.Exchange, "mappings", marshaledMsg)
	if err != nil {
		log.Debugln(err.Error())
		c.AbortWithStatusJSON(http.StatusInternalServerError, err.Error())

		return
	}

	c.Status(http.StatusOK)
}

func listActiveUsers(c *gin.Context) {
	users, err := Conf.API.DB.ListActiveUsers()
	if err != nil {
		log.Debugln("ListActiveUsers failed")
		c.AbortWithStatusJSON(http.StatusInternalServerError, err.Error())

		return
	}
	c.JSON(http.StatusOK, users)
}

func listUserFiles(c *gin.Context) {
	username := c.Param("username")
	username = strings.TrimPrefix(username, "/")
	username = strings.TrimSuffix(username, "/files")
	log.Debugln(username)
	files, err := Conf.API.DB.GetUserFiles(username)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, err.Error())

		return
	}

	c.Writer.Header().Set("Content-Type", "application/json")
	c.JSON(200, files)
}

// addC4ghHash handles the addition of a hashed public key to the database.
// It expects a JSON payload containing the base64 encoded public key and its description.
// If the JSON payload is invalid, it responds with a 400 Bad Request status.
// If the hash is already in the database, it responds with a 409 Conflict status
// If the database insertion fails, it responds with a 500 Internal Server Error status.
// On success, it responds with a 200 OK status.
func addC4ghHash(c *gin.Context) {
	var c4gh schema.C4ghPubKey
	if err := c.BindJSON(&c4gh); err != nil {
		c.AbortWithStatusJSON(
			http.StatusBadRequest,
			gin.H{
				"error":  "json decoding : " + err.Error(),
				"status": http.StatusBadRequest,
			},
		)

		log.Errorf("Invalid JSON payload: %v", err)

		return
	}

	b64d, err := base64.StdEncoding.DecodeString(c4gh.PubKey)
	if err != nil {
		c.AbortWithStatusJSON(
			http.StatusBadRequest,
			gin.H{
				"error":  "base64 decoding : " + err.Error(),
				"status": http.StatusBadRequest,
			},
		)

		log.Errorf("Invalid JSON payload: %v", err)

		return
	}

	pubKey, err := keys.ReadPublicKey(bytes.NewReader(b64d))
	if err != nil {
		c.AbortWithStatusJSON(
			http.StatusBadRequest,
			gin.H{
				"error":  "not a public key : " + err.Error(),
				"status": http.StatusBadRequest,
			},
		)

		log.Errorf("Invalid JSON payload: %v", err)

		return
	}

	err = Conf.API.DB.AddKeyHash(hex.EncodeToString(pubKey[:]), c4gh.Description)
	if err != nil {
		if strings.Contains(err.Error(), "key hash already exists") {
			c.AbortWithStatusJSON(
				http.StatusConflict,
				gin.H{
					"error":  err.Error(),
					"status": http.StatusConflict,
				},
			)
			log.Error("Key hash already exists")
		} else {
			c.AbortWithStatusJSON(
				http.StatusInternalServerError,
				gin.H{
					"error":  err.Error(),
					"status": http.StatusInternalServerError,
				},
			)
			log.Errorf("Database insertion failed: %v", err)
		}

		return
	}

	c.Status(http.StatusOK)
}

func listC4ghHashes(c *gin.Context) {
	hashes, err := Conf.API.DB.ListKeyHashes()
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, err.Error())

		return
	}

	for n, h := range hashes {
		ct, _ := time.Parse(time.RFC3339, h.CreatedAt)
		hashes[n].CreatedAt = ct.Format(time.DateTime)

		if h.DeprecatedAt != "" {
			dt, _ := time.Parse(time.RFC3339, h.DeprecatedAt)
			hashes[n].DeprecatedAt = dt.Format(time.DateTime)
		}
	}
	c.Writer.Header().Set("Content-Type", "application/json")
	c.JSON(200, hashes)
}

func deprecateC4ghHash(c *gin.Context) {
	keyHash := strings.TrimPrefix(c.Param("keyHash"), "/")
	err = Conf.API.DB.DeprecateKeyHash(keyHash)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, err.Error())

		return
	}
}
