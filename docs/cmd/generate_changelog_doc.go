package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"

	"golang.org/x/oauth2"
	"gopkg.in/yaml.v2"

	"github.com/spf13/cobra"

	"github.com/google/go-github/v31/github"
	"github.com/rotisserie/eris"
)

func main() {
	ctx := context.Background()
	app := rootApp(ctx)
	if err := app.Execute(); err != nil {
		fmt.Printf("unable to run: %v\n", err)
		os.Exit(1)
	}
}

type options struct {
	ctx              context.Context
	HugoDataSoloOpts HugoDataSoloOpts
}

type HugoDataSoloOpts struct {
	product string
	version string
	// if set, will override the version when rendering the
	callLatest bool
	noScope    bool
}

func rootApp(ctx context.Context) *cobra.Command {
	opts := &options{
		ctx: ctx,
	}
	app := &cobra.Command{
		Use: "docs-util",
		RunE: func(cmd *cobra.Command, args []string) error {

			return nil
		},
	}
	app.AddCommand(writeVersionScopeDataForHugo(opts))
	app.AddCommand(changelogMdFromGithubCmd(opts))

	app.PersistentFlags().StringVar(&opts.HugoDataSoloOpts.version, "version", "", "version of docs and code")
	app.PersistentFlags().StringVar(&opts.HugoDataSoloOpts.product, "product", "gloo", "product to which the docs refer (defaults to gloo)")
	app.PersistentFlags().BoolVar(&opts.HugoDataSoloOpts.noScope, "no-scope", false, "if set, will not nest the served docs by product or version")
	app.PersistentFlags().BoolVar(&opts.HugoDataSoloOpts.callLatest, "call-latest", false, "if set, will use the string 'latest' in the scope, rather than the particular release version")

	return app
}

func changelogMdFromGithubCmd(opts *options) *cobra.Command {
	app := &cobra.Command{
		Use:   "gen-changelog-md",
		Short: "generate a markdown file from Github Release pages API",
		RunE: func(cmd *cobra.Command, args []string) error {
			if os.Getenv(skipChangelogGeneration) != "" {
				return nil
			}
			return generateChangelogMd(args)
		},
	}
	return app
}

type HugoDataSoloYaml struct {
	// this is the string that is used to scope the hosted docs' urls
	// it can be either "latest" or a full semver version
	DocsVersion string `yaml:"DocsVersion"`
	// this is the string that tells the reader which version of code was used to publish the docs
	// it can only be a full semver version, unless user passes the --no-scope flag, which indicates "dev" mode
	CodeVersion string `yaml:"CodeVersion"`
}

const hugoDataSoloFilename = "data/Solo.yaml"

func writeVersionScopeDataForHugo(opts *options) *cobra.Command {
	app := &cobra.Command{
		Use:   "gen-version-scope-data",
		Short: "generate a data file for Hugo that indicates the docs version",
		RunE: func(cmd *cobra.Command, args []string) error {
			data := &HugoDataSoloYaml{}
			if err := getDocsVersionFromOpts(data, opts.HugoDataSoloOpts); err != nil {
				return err
			}
			marshalled, err := yaml.Marshal(data)
			if err != nil {
				return err
			}
			return ioutil.WriteFile(hugoDataSoloFilename, marshalled, 0644)
		},
	}
	return app
}

const (
	latestVersionPath = "latest"
)

func getDocsVersionFromOpts(soloData *HugoDataSoloYaml, hugoOpts HugoDataSoloOpts) error {
	if hugoOpts.noScope {
		soloData.CodeVersion = "dev"
		return nil
	}
	if hugoOpts.version == "" {
		return eris.New("must provide a version for scoped docs generation")
	}
	if hugoOpts.product == "" {
		return eris.New("must provide a product for scoped docs generation")
	}
	soloData.CodeVersion = hugoOpts.version
	version := hugoOpts.version
	if hugoOpts.callLatest {
		version = latestVersionPath
	}
	soloData.DocsVersion = fmt.Sprintf("/%v/%v", hugoOpts.product, version)
	return nil
}

const (
	glooDocGen              = "gloo"
	glooEDocGen             = "glooe"
	skipChangelogGeneration = "SKIP_CHANGELOG_GENERATION"
)

var (
	InvalidInputError = func(arg string) error {
		return eris.Errorf("invalid input, must provide exactly one argument, either '%v' or '%v', (provided %v)",
			glooDocGen,
			glooEDocGen,
			arg)
	}
	MissingGithubTokenError = func() error {
		return eris.Errorf("Must either set GITHUB_TOKEN or set %s environment variable to true", skipChangelogGeneration)
	}
)

func generateChangelogMd(args []string) error {
	if len(args) != 1 {
		return InvalidInputError(fmt.Sprintf("%v", len(args)-1))
	}
	client := github.NewClient(nil)
	target := args[0]
	var repo string
	switch target {
	case glooDocGen:
		repo = "gloo"
	case glooEDocGen:
		repo = "solo-projects"
		ctx := context.Background()
		if os.Getenv("GITHUB_TOKEN") == "" {
			return MissingGithubTokenError()
		}
		ts := oauth2.StaticTokenSource(
			&oauth2.Token{AccessToken: os.Getenv("GITHUB_TOKEN")},
		)
		tc := oauth2.NewClient(ctx, ts)
		client = github.NewClient(tc)
	default:
		return InvalidInputError(target)
	}

	allReleases, _, err := client.Repositories.ListReleases(context.Background(), "solo-io", repo,
		&github.ListOptions{
			Page:    0,
			PerPage: 10000000,
		})
	if err != nil {
		return err
	}

	for _, release := range allReleases {
		fmt.Printf("### %v\n\n", *release.TagName)
		fmt.Printf("%v", *release.Body)
	}
	return nil
}
