package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/charmbracelet/log"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/logger"
	"github.com/gofiber/fiber/v3/middleware/static"
	"github.com/golobby/config/v3"
	"github.com/golobby/config/v3/pkg/feeder"
	"resty.dev/v3"
)

type Config struct {
	Repositories []struct {
		Identifier string // need to validate
		BaseURL    string // need to validate
	}
}

var activeDownloads sync.Map

type RequestGroup struct {
	wg   sync.WaitGroup
	err  error
}

func main() {
	app := fiber.New()

	client := resty.New()
	defer client.Close()

	repoConfig := Config{}
	tomlFeeder := feeder.Toml{Path: "config.toml"}
	c := config.New()
	c.AddFeeder(tomlFeeder)
	c.AddStruct(&repoConfig)
	err := c.Feed()

	if err != nil {
		log.Fatal("Error parsing config", "err", err)
	}

	app.Use(logger.New(logger.Config{
		Format:     "${time} - ${status} - ${ip} ${method} ${path}\n",
		TimeFormat: "15:04:05 on 01/02/2006",
		TimeZone:   "America/Denver",
	}))

	err = os.Mkdir("./repos", 0777)
	if err != nil && !errors.Is(err, os.ErrExist) {
		log.Fatal("Error creating repository directory", "err", err)
	}

	for _, repository := range repoConfig.Repositories {
		repositoryBasePath := filepath.Join("./repos", repository.Identifier)
		err = os.Mkdir(repositoryBasePath, 0777)
		if err != nil && !errors.Is(err, os.ErrExist) {
			log.Fatal(
				"Error creating repository directory",
				"repo_id",
				repository.Identifier,
				"err",
				err,
			)
		}

		app.Use("/"+repository.Identifier, func(c fiber.Ctx) error {
			trimmedPath := strings.TrimPrefix(c.Path(), "/"+repository.Identifier)
			retrievalURL := repository.BaseURL + trimmedPath

			if _, err := os.Stat(filepath.Join(repositoryBasePath, trimmedPath)); err == nil {
				log.Info("Package in cache", "package", trimmedPath)
				return c.Next()
			}

			if actual, loaded := activeDownloads.Load(retrievalURL); loaded &&
				filepath.Base(trimmedPath) != "APKINDEX.tar.gz" {
				reqGroup := actual.(*RequestGroup)
				log.Info("Waiting for existing download", "package", trimmedPath)

				reqGroup.wg.Wait()

				if reqGroup.err != nil {
					return c.SendStatus(fiber.StatusBadGateway)
				}

				return c.Next()
			}

			reqGroup := &RequestGroup{}
			reqGroup.wg.Add(1)
			activeDownloads.Store(retrievalURL, reqGroup)

			defer func() {
				activeDownloads.Delete(retrievalURL)
				reqGroup.wg.Done()
			}()

			log.Info("Downloading Alpine Package", "package", trimmedPath)

			res, err := client.R().Get(retrievalURL)
			if err != nil {
				log.Error("Failed to download", "package", trimmedPath, "error", err)
				reqGroup.err = err
				return c.SendStatus(fiber.StatusBadGateway)
			}

			if res.StatusCode() != 200 {
				log.Error("Failed to download", "package", trimmedPath, "status", res.StatusCode())
				return c.SendStatus(res.StatusCode())
			}

			// return APKINDEX.tar.gz from memory, do not cache to disk
			if filepath.Base(trimmedPath) == "APKINDEX.tar.gz" {
				c.Attachment("APKINDEX.tar.gz")
				return c.SendStream(res.Body)
			}

			repositoryDirectory := filepath.Join(repositoryBasePath, filepath.Dir(trimmedPath))
			err = os.MkdirAll(repositoryDirectory, 0777)
			if err != nil {
				reqGroup.err = err
				log.Error(
					"Error creating repository directories",
					"path",
					repositoryDirectory,
					"err",
					err,
				)
			}

			packagePath := filepath.Join(repositoryBasePath, trimmedPath)
			err = os.WriteFile(packagePath, res.Bytes(), 0644)
			if err != nil {
				reqGroup.err = err
				log.Error(
					"Error writing package",
					"path",
					packagePath,
					"err",
					err,
				)
			}

			return c.Next()
		}, static.New(repositoryBasePath))
	}

	err = app.Listen(":3000")
	if err != nil {
		log.Fatal("Failed to start server", "err", err)
	}
}
