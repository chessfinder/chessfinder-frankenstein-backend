package main

import (
	"encoding/json"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/dynamodb/dynamodbattribute"
	"github.com/aws/aws-sdk-go/service/sqs"
	"github.com/chessfinder/chessfinder-faster-backend/src_go/details/api"
	"github.com/chessfinder/chessfinder-faster-backend/src_go/details/db/archives"
	"github.com/chessfinder/chessfinder-faster-backend/src_go/details/db/searches"
	"github.com/chessfinder/chessfinder-faster-backend/src_go/details/db/users"
	"github.com/chessfinder/chessfinder-faster-backend/src_go/details/queue"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/chessfinder/chessfinder-faster-backend/src_go/search/initiate/validation"
)

type SearchRegistrar struct {
	userTableName       string
	archivesTableName   string
	searchesTableName   string
	searchBoardQueueUrl string
	awsConfig           *aws.Config
}

func (registrar *SearchRegistrar) RegisterSearchRequest(event *events.APIGatewayV2HTTPRequest) (responseEvent events.APIGatewayV2HTTPResponse, err error) {
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

	awsSession, err := session.NewSession(registrar.awsConfig)
	if err != nil {
		logger.Panic("impossible to create an AWS session!")
		return
	}
	dynamodbClient := dynamodb.New(awsSession)
	svc := sqs.New(awsSession)

	method := event.RequestContext.HTTP.Method
	path := event.RequestContext.HTTP.Path

	if path != "/api/faster/board" || method != "POST" {
		logger.Panic("search request registrar is attached to a wrong route!")
		panic("not supported")
	}

	logger = logger.With(zap.String("method", method), zap.String("path", path))

	searchRequest := SearchRequest{}
	err = json.Unmarshal([]byte(event.Body), &searchRequest)
	if err != nil {
		logger.Error("error while unmarshalling search request", zap.Error(err), zap.String("body", event.Body))
		err = api.InvalidBody
	}

	logger = logger.With(zap.String("username", searchRequest.Username), zap.String("platform", searchRequest.Platform))
	logger = logger.With(zap.String("board", searchRequest.Board))

	logger.Info("validating board")
	if isValid, strangeError := validation.ValidateBoard(searchRequest.Board); !isValid || strangeError != nil {
		logger.Info("invalid board")
		if strangeError != nil {
			logger.Error("error while validating board", zap.Error(strangeError))
		}
		err = InvalidSearchBoard
		return
	}

	logger.Info("fetching user from db", zap.String("user", searchRequest.Username))
	user, err := registrar.getUserRecord(searchRequest, logger, dynamodbClient)
	if err != nil {
		return
	}

	logger = logger.With(zap.String("userId", user.UserId))

	logger.Info("fetching archives from db")
	archives, err := registrar.getArchiveRecords(user, logger, dynamodbClient)
	if err != nil {
		return
	}

	downloadedGames := 0
	for _, archive := range archives {
		downloadedGames += archive.Downloaded
	}

	if downloadedGames == 0 {
		logger.Info("no game available", zap.String("user", user.UserId))
		err = NoGameAvailable(user.Username)
		return
	}

	searchId := uuid.New().String()
	logger = logger.With(zap.String("searchResultId", searchId))
	now := time.Now()

	searchResult := searches.NewSearchRecord(searchId, now, downloadedGames)

	logger.Info("putting search result")
	err = registrar.persistSearchRecord(dynamodbClient, logger, searchResult)
	if err != nil {
		return
	}

	logger.Info("sending search board command")

	searchBoardCommand := queue.SearchBoardCommand{
		UserId:   user.UserId,
		SearchId: searchId,
		Board:    searchRequest.Board,
	}

	searchBoardCommandJson, err := json.Marshal(searchBoardCommand)
	if err != nil {
		logger.Error("error while marshalling search board command")
	}

	sendMessageInput := &sqs.SendMessageInput{
		MessageBody: aws.String(string(searchBoardCommandJson)),
		QueueUrl:    aws.String(registrar.searchBoardQueueUrl),
		//fixme this should be the boeard, but that makes the test flaky. in test we need to wait for the message to be processed and forgotten by SQS. To overcome this we should generate valid boear each time. That will break the restriction of deduplication.
		MessageDeduplicationId: aws.String(searchBoardCommand.SearchId),
		MessageGroupId:         aws.String(user.UserId),
	}

	_, err = svc.SendMessage(sendMessageInput)
	if err != nil {
		logger.Error("error while sending search board command")
	}

	logger.Info("search board command sent")

	searchResponse := SearchResponse{
		SearchId: searchId,
	}

	searchResponseJson, err := json.Marshal(searchResponse)
	if err != nil {
		logger.Error("error while marshalling search response")
	}

	responseEvent = events.APIGatewayV2HTTPResponse{
		StatusCode: 200,
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
		Body: string(searchResponseJson),
	}

	return
}

func (registrar *SearchRegistrar) getUserRecord(
	searchRequest SearchRequest,
	logger *zap.Logger,
	dynamodbClient *dynamodb.DynamoDB,
) (user users.UserRecord, err error) {
	getItemOutput, err := dynamodbClient.GetItem(
		&dynamodb.GetItemInput{
			TableName: aws.String(registrar.userTableName),
			Key: map[string]*dynamodb.AttributeValue{
				"username": {
					S: aws.String(searchRequest.Username),
				},
				"platform": {
					S: aws.String(string(searchRequest.Platform)),
				},
			},
		},
	)

	if err != nil {
		logger.Error("error while getting user from db", zap.Error(err))
		return
	}
	if len(getItemOutput.Item) == 0 {
		err = ProfileIsNotCached(searchRequest.Username, searchRequest.Platform)
		logger.Info("profile is not cached")
		return
	}
	err = dynamodbattribute.UnmarshalMap(getItemOutput.Item, &user)
	if err != nil {
		logger.Error("error while unmarshalling user from db", zap.Error(err))
		return
	}
	return
}

func (registrar *SearchRegistrar) getArchiveRecords(
	user users.UserRecord,
	logger *zap.Logger,
	dynamodbClient *dynamodb.DynamoDB,
) (archives []archives.ArchiveRecord, err error) {
	getItemOutput, err := dynamodbClient.Query(&dynamodb.QueryInput{
		TableName:              aws.String(registrar.archivesTableName),
		KeyConditionExpression: aws.String("user_id = :user_id"),
		ExpressionAttributeValues: map[string]*dynamodb.AttributeValue{
			":user_id": {
				S: aws.String(user.UserId),
			},
		},
	})
	if err != nil {
		logger.Error("error while getting archives from db", zap.Error(err))
		return
	}
	if getItemOutput.Items == nil {
		logger.Info("no archives found for user")
		return
	}
	err = dynamodbattribute.UnmarshalListOfMaps(getItemOutput.Items, &archives)
	if err != nil {
		logger.Error("error while unmarshalling archives from db", zap.Error(err))
		return
	}
	return
}

func (registrar *SearchRegistrar) persistSearchRecord(
	dynamodbClient *dynamodb.DynamoDB,
	logger *zap.Logger,
	search searches.SearchRecord,
) (err error) {

	searchRecordItems, err := dynamodbattribute.MarshalMap(search)
	if err != nil {
		logger.Error("error while marshalling search record", zap.Error(err))
		return
	}

	_, err = dynamodbClient.PutItem(&dynamodb.PutItemInput{
		TableName: aws.String(registrar.searchesTableName),
		Item:      searchRecordItems,
	})

	if err != nil {
		logger.Error("error while putting search record", zap.Error(err))
		return
	}

	return
}
