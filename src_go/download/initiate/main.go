package main

import (
	"errors"
	"os"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/chessfinder/chessfinder-faster-backend/src_go/details/api"
)

func main() {

	downloadsTableName, downloadsTableNameExists := os.LookupEnv("DOWNLOADS_TABLE_NAME")
	if !downloadsTableNameExists {
		panic(errors.New("DOWNLOADS_TABLE_NAME is missing"))
	}

	archivesTableName, archivesTableNameExists := os.LookupEnv("ARCHIVES_TABLE_NAME")
	if !archivesTableNameExists {
		panic(errors.New("ARCHIVES_TABLE_NAME is missing"))
	}

	usersTableName, usersTableNameExists := os.LookupEnv("USERS_TABLE_NAME")
	if !usersTableNameExists {
		panic(errors.New("USERS_TABLE_NAME is missing"))
	}

	chessDotComUrl, chessDotComUrlExists := os.LookupEnv("CHESS_DOT_COM_URL")
	if !chessDotComUrlExists {
		panic(errors.New("CHESS_DOT_COM_URL is missing"))
	}

	downloadGamesQueueUrl, downloadGamesQueueUrlExists := os.LookupEnv("DOWNLOAD_GAMES_QUEUE_URL")
	if !downloadGamesQueueUrlExists {
		panic(errors.New("DOWNLOAD_GAMES_QUEUE_URL is missing"))
	}

	awsRegion, awsRegionExists := os.LookupEnv("AWS_REGION")
	if !awsRegionExists {
		panic(errors.New("AWS_REGION is missing"))
	}

	checker := ArchiveDownloader{
		downloadsTableName:    downloadsTableName,
		usersTableName:        usersTableName,
		archivesTableName:     archivesTableName,
		downloadGamesQueueUrl: downloadGamesQueueUrl,
		chessDotComUrl:        chessDotComUrl,
		awsConfig: &aws.Config{
			Region: &awsRegion,
		},
	}

	lambda.Start(api.WithRecover(checker.DownloadArchiveAndDistributeDonwloadGameCommands))
}
