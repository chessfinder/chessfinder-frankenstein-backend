package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/dynamodb/dynamodbattribute"
	"github.com/aws/aws-sdk-go/service/sqs"
	"github.com/chessfinder/chessfinder-faster-backend/src_go/details/api"
	"github.com/chessfinder/chessfinder-faster-backend/src_go/details/batcher"
	"github.com/chessfinder/chessfinder-faster-backend/src_go/details/db/archives"
	"github.com/chessfinder/chessfinder-faster-backend/src_go/details/db/downloads"
	"github.com/chessfinder/chessfinder-faster-backend/src_go/details/db/users"
	"github.com/chessfinder/chessfinder-faster-backend/src_go/details/queue"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type ArchiveDownloader struct {
	chessDotComUrl        string
	awsConfig             *aws.Config
	usersTableName        string
	archivesTableName     string
	downloadsTableName    string
	downloadGamesQueueUrl string
}

func (downloader *ArchiveDownloader) DownloadArchiveAndDistributeDonwloadGameCommands(
	event *events.APIGatewayV2HTTPRequest,
) (responseEvent events.APIGatewayV2HTTPResponse, err error) {
	config := zap.NewProductionConfig()
	config.OutputPaths = []string{"stdout"}
	timeEncoder := func(t time.Time, enc zapcore.PrimitiveArrayEncoder) {
		enc.AppendString(t.UTC().Format("2006-01-02T15:04:05.000Z"))
	} // Log to stdout
	config.EncoderConfig.EncodeTime = timeEncoder

	// Create the logger from the configuration
	logger, err := config.Build()
	if err != nil {
		panic(err)
	}
	logger = logger.With(zap.String("requestId", event.RequestContext.RequestID))
	defer logger.Sync()

	awsSession, err := session.NewSession(downloader.awsConfig)
	if err != nil {
		logger.Panic("impossible to create an AWS session!")
		return
	}
	dynamodbClient := dynamodb.New(awsSession)
	svc := sqs.New(awsSession)
	chessDotComClient := &http.Client{}

	method := event.RequestContext.HTTP.Method
	path := event.RequestContext.HTTP.Path

	if path != "/api/faster/game" || method != "POST" {
		logger.Panic("archive downloader is attached to a wrong route!")
	}
	downloadRequest := DownloadRequest{}
	err = json.Unmarshal([]byte(event.Body), &downloadRequest)
	if err != nil {
		logger.Error("impossible to unmarshal the request body!")
		err = api.InvalidBody
		return
	}

	logger = logger.With(zap.String("username", downloadRequest.Username), zap.String("platform", downloadRequest.Platform))

	profile, err := downloader.getAndPersistUser(dynamodbClient, chessDotComClient, logger, downloadRequest)
	if err != nil {
		return
	}

	logger = logger.With(zap.String("userId", profile.UserId))

	archivesFromChessDotCom, err := downloader.getArchivesFromChessDotCom(logger, profile)
	if err != nil {
		return
	}

	archivesFromDb, err := downloader.getArchivesFromDb(dynamodbClient, logger, profile)
	if err != nil {
		return
	}

	missingArchiveUrls := resolveMissingArchives(archivesFromChessDotCom, archivesFromDb)
	missingArchives, err := downloader.persistMissingArchives(dynamodbClient, logger, profile, missingArchiveUrls)
	if err != nil {
		return
	}

	archivesToDownload := resolveArchivesToDownload(archivesFromDb)
	downloadId := uuid.New().String()
	downloadRecord := downloads.NewDownloadRecord(downloadId, len(missingArchives)+len(archivesToDownload))

	downloadRecorItems, err := dynamodbattribute.MarshalMap(downloadRecord)
	if err != nil {
		logger.Error("impossible to marshal the download record!", zap.Error(err))
		return
	}
	_, err = dynamodbClient.PutItem(&dynamodb.PutItemInput{
		TableName: aws.String(downloader.downloadsTableName),
		Item:      downloadRecorItems,
	})

	if err != nil {
		logger.Error("impossible to persist the download record!", zap.Error(err))
		return
	}

	err = downloader.publishDownloadGameCommands(logger, svc, profile, downloadRecord, missingArchives, archivesToDownload)
	if err != nil {
		return
	}

	downloadResponse := DownloadResponse{
		DownloadId: downloadId,
	}

	jsonBody, err := json.Marshal(downloadResponse)
	if err != nil {
		logger.Error("impossible to marshal the download response!", zap.Error(err))
		return
	}

	responseEvent = events.APIGatewayV2HTTPResponse{
		StatusCode: 200,
		Body:       string(jsonBody),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
	}

	return
}

func (downloader ArchiveDownloader) getAndPersistUser(
	dynamodbClient *dynamodb.DynamoDB,
	chessDotComClient *http.Client,
	logger *zap.Logger,
	downloadRequest DownloadRequest,
) (userRecord users.UserRecord, err error) {
	url := downloader.chessDotComUrl + "/pub/player/" + downloadRequest.Username
	logger = logger.With(zap.String("url", url))

	request, err := http.NewRequest("GET", url, bytes.NewBuffer([]byte{}))

	if err != nil {
		logger.Error("impossible to create a request to chess.com!")
		return
	}

	logger.Info("requesting chess.com for profile")
	response, err := chessDotComClient.Do(request)
	if err != nil {
		return
	}

	defer response.Body.Close()

	responseBodyBytes, err := io.ReadAll(response.Body)
	if err != nil {
		logger.Error("impossible to read the response body from chess.com!", zap.Error(err))
		return
	}

	responseBodyString := string(responseBodyBytes)

	if response.StatusCode == 404 {
		logger.Error("profile not found on chess.com!", zap.String("responseBody", responseBodyString))
		err = ProfileNotFound(downloadRequest)
		return
	}

	if response.StatusCode != 200 {
		logger.Error("unexpected status code from chess.com!", zap.Int("statusCode", response.StatusCode), zap.String("responseBody", responseBodyString))
		err = api.ServiceOverloaded
		return
	}

	profile := ChessDotComProfile{}
	err = json.Unmarshal([]byte(responseBodyString), &profile)
	if err != nil {
		logger.Error("impossible to unmarshal the response body from chess.com!", zap.Error(err), zap.String("responseBody", responseBodyString), zap.String("url", url))
		return
	}

	logger.Info("profile found!")

	userRecord = users.UserRecord{
		Username: downloadRequest.Username,
		UserId:   profile.UserId,
		Platform: users.ChessDotCom,
	}

	userRecordItems, err := dynamodbattribute.MarshalMap(userRecord)

	if err != nil {
		logger.Error("impossible to marshal the user!", zap.Error(err))
		return
	}

	_, err = dynamodbClient.PutItem(&dynamodb.PutItemInput{
		TableName: aws.String(downloader.usersTableName),
		Item:      userRecordItems,
	})

	if err != nil {
		logger.Error("impossible to persist the user!", zap.Error(err))
		return
	}
	logger.Info("user persisted")

	return
}

func (downloader ArchiveDownloader) getArchivesFromChessDotCom(
	logger *zap.Logger,
	user users.UserRecord,
) (archives ChessDotComArchives, err error) {
	url := downloader.chessDotComUrl + "/pub/player/" + user.Username + "/games/archives"
	logger = logger.With(zap.String("url", url))
	logger.Info("requesting chess.com for archives")
	request, err := http.NewRequest("GET", url, bytes.NewBuffer([]byte{}))
	if err != nil {
		logger.Error("impossible to create a request to chess.com!")
		return
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		logger.Error("impossible to request chess.com!")
		return
	}

	defer response.Body.Close()

	responseBodyBytes, err := io.ReadAll(response.Body)
	if err != nil {
		logger.Error("impossible to read the response body from chess.com!", zap.Error(err))
		return
	}

	responseBodyString := string(responseBodyBytes)

	if response.StatusCode != 200 {
		logger.Error("unexpected status code from chess.com", zap.Int("statusCode", response.StatusCode), zap.String("responseBody", responseBodyString))
		err = api.ServiceOverloaded
		return
	}

	archives = ChessDotComArchives{}
	err = json.Unmarshal([]byte(responseBodyString), &archives)
	if err != nil {
		logger.Error("impossible to unmarshal the response body from chess.com!", zap.Error(err), zap.String("responseBody", responseBodyString))
		return
	}

	logger.Info("archives found from chess.com", zap.Int("existingArchivesCount", len(archives.Archives)))

	return
}

func (downloader ArchiveDownloader) getArchivesFromDb(
	dynamodbClient *dynamodb.DynamoDB,
	logger *zap.Logger,
	user users.UserRecord,
) (archiveRecords []archives.ArchiveRecord, err error) {
	logger.Info("requesting dynamodb for archives")
	response, err := dynamodbClient.Query(&dynamodb.QueryInput{
		TableName: aws.String(downloader.archivesTableName),
		KeyConditions: map[string]*dynamodb.Condition{
			"user_id": {
				ComparisonOperator: aws.String("EQ"),
				AttributeValueList: []*dynamodb.AttributeValue{
					{
						S: aws.String(user.UserId),
					},
				},
			},
		},
	})
	if err != nil {
		logger.Error("impossible to query dynamodb for archives!", zap.Error(err))
		return
	}

	archiveRecords = make([]archives.ArchiveRecord, len(response.Items))
	for i, item := range response.Items {
		archive := archives.ArchiveRecord{}
		err = dynamodbattribute.UnmarshalMap(item, &archive)
		if err != nil {
			logger.Error("impossible to unmarshal the archive!", zap.Error(err))
			return
		}
		archiveRecords[i] = archive
	}
	logger.Info("archives found from database", zap.Int("totalArchivesCount", len(archiveRecords)))

	return
}

func resolveMissingArchives(
	archivesFromChessDotCom ChessDotComArchives,
	archivesFromDb []archives.ArchiveRecord,
) (missingArchives []string) {
	existingArchives := make(map[string]archives.ArchiveRecord, len(archivesFromDb))
	for _, archiveFromDb := range archivesFromDb {
		existingArchives[archiveFromDb.ArchiveId] = archiveFromDb
	}

	missingArchives = make([]string, 0)
	for _, archiveFromChessDotCom := range archivesFromChessDotCom.Archives {
		if _, ok := existingArchives[archiveFromChessDotCom]; !ok {
			missingArchives = append(missingArchives, archiveFromChessDotCom)
		}
	}
	return
}

func resolveArchivesToDownload(
	archivesFromDb []archives.ArchiveRecord,
) (archivesToDownload []archives.ArchiveRecord) {
	archivesToDownload = make([]archives.ArchiveRecord, 0)
	for _, archiveFromDb := range archivesFromDb {
		if archiveFromDb.DownloadedAt == nil {
			archivesToDownload = append(archivesToDownload, archiveFromDb)
			continue
		}
		archiveHasGamesTill := time.Date(archiveFromDb.Year, time.Month(archiveFromDb.Month), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
		if archiveFromDb.DownloadedAt.ToTime().Before(archiveHasGamesTill) {
			archivesToDownload = append(archivesToDownload, archiveFromDb)
		}
	}
	return
}

func (downloader ArchiveDownloader) persistMissingArchives(
	dynamodbClient *dynamodb.DynamoDB,
	logger *zap.Logger,
	user users.UserRecord,
	missingArchiveUrls []string,
) (missingArchiveRecords []archives.ArchiveRecord, err error) {
	logger.Info("persisting missing archives", zap.Int("missingArchivesCount", len(missingArchiveUrls)))

	for _, missingArchiveUrl := range missingArchiveUrls {
		logger := logger.With(zap.String("archiveId", missingArchiveUrl))
		archiveSegments := strings.Split(missingArchiveUrl, "/")
		maybeYear := archiveSegments[len(archiveSegments)-2]
		maybeMonth := archiveSegments[len(archiveSegments)-1]

		var year int
		year, err = strconv.Atoi(maybeYear)
		if err != nil {
			logger.Error("impossible to parse the year!", zap.Error(err))
			return
		}

		var month int
		month, err = strconv.Atoi(maybeMonth)
		if err != nil {
			logger.Error("impossible to parse the month!", zap.Error(err))
			return
		}

		missingArchiveRecord := archives.ArchiveRecord{
			UserId:       user.UserId,
			ArchiveId:    missingArchiveUrl,
			Resource:     missingArchiveUrl,
			Year:         year,
			Month:        month,
			Downloaded:   0,
			DownloadedAt: nil,
		}

		missingArchiveRecords = append(missingArchiveRecords, missingArchiveRecord)
	}

	missingArchiveRecordWriteRequests := make([]*dynamodb.WriteRequest, len(missingArchiveRecords))
	for i, missingArchiveRecord := range missingArchiveRecords {
		var missingArchiveRecordItems map[string]*dynamodb.AttributeValue
		missingArchiveRecordItems, err = dynamodbattribute.MarshalMap(missingArchiveRecord)
		if err != nil {
			logger.Error("impossible to marshal the archive!", zap.Error(err))
			return nil, err
		}

		writeRequest := dynamodb.WriteRequest{
			PutRequest: &dynamodb.PutRequest{
				Item: missingArchiveRecordItems,
			},
		}

		missingArchiveRecordWriteRequests[i] = &writeRequest
	}

	missingArchiveRecordWriteRequestsMatrix := batcher.Batcher(missingArchiveRecordWriteRequests, 25)

	logger.Info("persisting missing archives in batches", zap.Int("batchesCount", len(missingArchiveRecordWriteRequestsMatrix)))

	for batchNumber, batch := range missingArchiveRecordWriteRequestsMatrix {
		logger := logger.With(zap.Int("batchNumber", batchNumber+1), zap.Int("batchSize", len(batch)))
		logger.Info("trying to persist a batch of missing archives")

		unprocessedWriteRequests := map[string][]*dynamodb.WriteRequest{
			downloader.archivesTableName: batch,
		}

		for len(unprocessedWriteRequests) > 0 {
			logger.Info("trying to persist missing archives in one iteration")
			var writeOutput *dynamodb.BatchWriteItemOutput
			writeOutput, err = dynamodbClient.BatchWriteItem(&dynamodb.BatchWriteItemInput{
				RequestItems: unprocessedWriteRequests,
			})

			if err != nil {
				logger.Error("impossible to persist the missing archive records", zap.Error(err))
				return
			}

			unprocessedWriteRequests = writeOutput.UnprocessedItems
			time.Sleep(time.Millisecond * 100)
		}
	}

	logger.Info("missing archives persisted", zap.Int("persistedArchivesCount", len(missingArchiveUrls)))
	return
}

func (downloader ArchiveDownloader) publishDownloadGameCommands(
	logger *zap.Logger,
	svc *sqs.SQS,
	user users.UserRecord,
	downloadRecords downloads.DownloadRecord,
	missingArchives []archives.ArchiveRecord,
	shouldBeDownloadedArchives []archives.ArchiveRecord,
) (err error) {
	allArchives := append(shouldBeDownloadedArchives, missingArchives...)
	logger = logger.With(zap.Int("eligibleForDownloadArchivesCount", len(allArchives)))
	logger.Info("publishing download game commands ...")

	for _, archive := range allArchives {
		logger := logger.With(zap.String("archiveId", archive.ArchiveId))
		command := queue.DownloadGamesCommand{
			Username:   user.Username,
			Platform:   "CHESS_DOT_COM",
			ArchiveId:  archive.ArchiveId,
			UserId:     archive.UserId,
			DownloadId: downloadRecords.DownloadId,
		}
		var jsonBody []byte
		jsonBody, err = json.Marshal(command)
		if err != nil {
			logger.Error("impossible to marshal the download game command!", zap.Error(err))
			return err
		}

		_, err = svc.SendMessage(&sqs.SendMessageInput{
			QueueUrl:               aws.String(downloader.downloadGamesQueueUrl),
			MessageBody:            aws.String(string(jsonBody)),
			MessageDeduplicationId: aws.String(archive.ArchiveId),
			MessageGroupId:         aws.String(archive.UserId),
		})
		if err != nil {
			logger.Error("impossible to publish the download game command!", zap.Error(err))
			return
		}
	}

	logger.Info("download game commands published")
	return
}
