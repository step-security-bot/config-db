package cmd

import (
	"context"
	"fmt"
	"net/url"

	"github.com/flanksource/commons/logger"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/spf13/cobra"

	"github.com/flanksource/config-db/api"
	v1 "github.com/flanksource/config-db/api/v1"
	"github.com/flanksource/config-db/db"
	"github.com/flanksource/config-db/jobs"
	"github.com/flanksource/config-db/query"
	dutyContext "github.com/flanksource/duty/context"

	"github.com/flanksource/config-db/scrapers"
)

// Serve ...
var Serve = &cobra.Command{
	Use: "serve",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		db.MustInit(ctx)

		api.DefaultContext = api.NewScrapeContext(ctx, db.DefaultDB(), db.Pool)
		if err := dutyContext.LoadPropertiesFromFile(api.DefaultContext.DutyContext(), propertiesFile); err != nil {
			return fmt.Errorf("failed to load properties: %v", err)
		}

		serve(ctx, args)
		return nil
	},
}

func serve(ctx context.Context, configFiles []string) {

	e := echo.New()
	// PostgREST needs to know how it is exposed to create the correct links
	db.HTTPEndpoint = publicEndpoint + "/db"

	if logger.IsTraceEnabled() {
		e.Use(middleware.Logger())
	}
	if !disablePostgrest {
		go db.StartPostgrest()
		forward(e, "/db", db.PostgRESTEndpoint())
		forward(e, "/live", db.PostgRESTAdminEndpoint())
		forward(e, "/ready", db.PostgRESTAdminEndpoint())
	} else {
		e.GET("/live", func(c echo.Context) error {
			return c.String(200, "OK")
		})

		e.GET("/ready", func(c echo.Context) error {
			return c.String(200, "OK")
		})
	}

	e.GET("/query", query.Handler)
	e.POST("/run/:id", scrapers.RunNowHandler)

	go startScraperCron(configFiles)

	go jobs.ScheduleJobs(api.DefaultContext.DutyContext())

	go func() {
		if err := e.Start(fmt.Sprintf(":%d", httpPort)); err != nil {
			e.Logger.Fatal(err)
		}
	}()

	<-ctx.Done()
	if err := db.StopEmbeddedPGServer(); err != nil {
		logger.Errorf("failed to stop server: %v", err)
	}

	if err := e.Shutdown(ctx); err != nil {
		logger.Errorf("failed to shutdown echo HTTP server: %v", err)
	}
}

func startScraperCron(configFiles []string) {
	scraperConfigsFiles, err := v1.ParseConfigs(configFiles...)
	if err != nil {
		logger.Fatalf("error parsing config files: %v", err)
	}

	logger.Infof("Persisting %d config files", len(scraperConfigsFiles))
	for _, scrapeConfig := range scraperConfigsFiles {
		_, err := db.PersistScrapeConfigFromFile(scrapeConfig)
		if err != nil {
			logger.Fatalf("Error persisting scrape config to db: %v", err)
		}
	}

	scrapers.SyncScrapeConfigs(api.DefaultContext)

}

func forward(e *echo.Echo, prefix string, target string) {
	targetURL, err := url.Parse(target)
	if err != nil {
		e.Logger.Fatal(err)
	}
	e.Group(prefix).Use(middleware.ProxyWithConfig(middleware.ProxyConfig{
		Rewrite: map[string]string{
			fmt.Sprintf("^%s/*", prefix): "/$1",
		},
		Balancer: middleware.NewRoundRobinBalancer([]*middleware.ProxyTarget{
			{
				URL: targetURL,
			},
		}),
	}))
}

func init() {
	ServerFlags(Serve.Flags())
}
