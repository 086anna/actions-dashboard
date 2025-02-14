package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"text/template"
	"time"

	"golang.org/x/term"

	"github.com/charmbracelet/lipgloss"
	"github.com/cli/safeexec"
	flag "github.com/spf13/pflag"
	"github.com/vilmibm/actions-dashboard/util"
)

const defaultMaxRuns = 5
const defaultWorkflowNameLength = 17
const defaultApiCacheTime = "60m"

type run struct {
	Finished   time.Time
	Elapsed    time.Duration
	Status     string
	Conclusion string
	URL        string
}

type workflow struct {
	Name       string
	Runs       []run
	BillableMs int
}

func (w *workflow) RenderHealth() string {
	successStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#32cd32"))
	neutralStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#808080"))
	failedStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#dc143c"))
	var results string

	for i, r := range w.Runs {
		if i > defaultMaxRuns {
			break
		}

		if r.Status != "completed" {
			results += neutralStyle.Render("-")
			continue
		}

		switch r.Conclusion {
		case "success":
			results += successStyle.Render("✓")
		case "skipped", "cancelled", "neutral":
			results += neutralStyle.Render("-")
		default:
			results += failedStyle.Render("x")
		}
	}

	return results
}

func (w *workflow) AverageElapsed() time.Duration {
	var totalTime int
	var averageTime int

	for i, r := range w.Runs {
		if i > defaultMaxRuns {
			break
		}

		totalTime += int(r.Elapsed.Seconds())
	}

	averageTime = totalTime / defaultMaxRuns

	s := fmt.Sprintf("%ds", averageTime)
	d, _ := time.ParseDuration(s)

	return d
}

func truncateWorkflowName(name string, length int) string {
	if len(name) > length {
		return name[:length] + "..."
	}

	return name
}

func getTerminalWidth() int {
	width, _, err := term.GetSize(int(os.Stdout.Fd()))

	if err != nil {
		panic(err.Error())
	}

	return width
}

func (w *workflow) RenderCard() string {
	workflowNameStyle := lipgloss.NewStyle().Bold(true)
	labelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#808080"))
	var tmpl *template.Template
	tmplData := struct {
		Name       string
		AvgElapsed time.Duration
		Health     string
		BillableMs int
		PrettyMS   func(int) string
		Label      func(string) string
	}{
		Name:       workflowNameStyle.Render(truncateWorkflowName(w.Name, defaultWorkflowNameLength)),
		AvgElapsed: w.AverageElapsed(),
		Health:     w.RenderHealth(),
		BillableMs: w.BillableMs,
		PrettyMS:   util.PrettyMS,
		Label: func(s string) string {
			return labelStyle.Render(s)
		},
	}

	// Assumes that run data is time filtered already
	// TODO add color etc in here:
	if len(w.Runs) == 0 {
		tmpl, _ = template.New("emptyWorkflowCard").Parse(
			`{{ .Name }}
{{call .Label "No runs"}}`)
	} else {
		tmpl, _ = template.New("workflowCard").Parse(
			`{{ .Name }}
{{call .Label "Health:"}} {{ .Health }}
{{call .Label "Avg elapsed:"}} {{ .AvgElapsed }}
{{- if .BillableMs }}
{{call .Label "Billable time:"}} {{call .PrettyMS .BillableMs }}{{end}}`)
	}
	buf := bytes.Buffer{}
	_ = tmpl.Execute(&buf, tmplData)
	return buf.String()
}

type repositoryData struct {
	Name      string `json:"full_name"`
	Private   bool
	Workflows []*workflow
}

type options struct {
	Repositories []string
	Last         time.Duration
	Selector     string
}

func _main(opts *options) error {
	selector := opts.Selector
	last := opts.Last

	repos, err := populateRepos(opts)
	if err != nil {
		return fmt.Errorf("could not fetch repository data: %w", err)
	}

	columnWidth := defaultWorkflowNameLength + 5 // account for ellipsis and padding/border
	cardsPerRow := (getTerminalWidth() / columnWidth) - 1

	cardStyle := lipgloss.NewStyle().
		Align(lipgloss.Left).
		Padding(1).
		Width(columnWidth).
		BorderStyle(lipgloss.DoubleBorder()).
		BorderForeground(lipgloss.Color("63"))

	titleStyle := lipgloss.NewStyle().Bold(true).Align(lipgloss.Center).Width(getTerminalWidth())
	subTitleStyle := lipgloss.NewStyle().Align(lipgloss.Center).Width(getTerminalWidth())
	repoNameStyle := lipgloss.NewStyle().Bold(true)
	repoHintStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#808080")).Italic(true)

	totalBillableMs := 0

	for _, r := range repos {
		workflows, err := getWorkflows(*r, last)
		if err != nil {
			return err
		}

		r.Workflows = workflows

		for _, w := range workflows {
			totalBillableMs += w.BillableMs
		}
	}

	fmt.Println(titleStyle.Render(fmt.Sprintf("GitHub Actions dashboard for %s for the past %s", selector, util.FuzzyAgo(opts.Last))))
	fmt.Println(subTitleStyle.Render(fmt.Sprintf("Total billable time: %s", util.PrettyMS(totalBillableMs))))

	for _, r := range repos {
		if len(r.Workflows) == 0 {
			continue
		}
		fmt.Println()
		fmt.Print(repoNameStyle.Render(r.Name))
		// TODO leverage go-gh to determine what host to use
		// (NB: go-gh needs a PR in order to help with this)
		fmt.Print(repoHintStyle.Render(fmt.Sprintf(" https://github.com/%s/actions\n", r.Name)))
		fmt.Println()

		totalRows := int(math.Ceil(float64(len(r.Workflows)) / float64(cardsPerRow)))
		cardRows := make([][]string, totalRows)
		rowIndex := 0

		for _, w := range r.Workflows {
			if len(cardRows[rowIndex]) == cardsPerRow {
				rowIndex++
			}

			cardRows[rowIndex] = append(cardRows[rowIndex], cardStyle.Render(w.RenderCard()))
		}

		for _, row := range cardRows {
			fmt.Println(lipgloss.JoinHorizontal(lipgloss.Top, row...))
		}
	}

	return nil
}

func populateRepos(opts *options) ([]*repositoryData, error) {
	result := []*repositoryData{}
	if len(opts.Repositories) > 0 {
		for _, repoName := range opts.Repositories {
			repoData, err := getRepo(opts.Selector, repoName)
			if err != nil {
				return nil, fmt.Errorf("failed to fetch data for %s/%s: %w", opts.Selector, repoName, err)
			}
			result = append(result, repoData)
		}

		return result, nil
	}

	var orgErr error
	var userErr error
	result, orgErr = getAllRepos(fmt.Sprintf("orgs/%s/repos", opts.Selector))
	if orgErr != nil {
		result, userErr = getAllRepos(fmt.Sprintf("users/%s/repos", opts.Selector))
		if userErr != nil {
			return nil, fmt.Errorf("could not find a user or org called '%s': %s; %s", opts.Selector, orgErr, userErr)
		}
	}

	return result, nil
}

func getRepo(owner, name string) (*repositoryData, error) {
	path := fmt.Sprintf("repos/%s/%s", owner, name)
	var stdout bytes.Buffer
	var data repositoryData
	var err error
	// TODO consider using go-gh
	if stdout, _, err = gh("api", "--cache", defaultApiCacheTime, path); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(stdout.Bytes(), &data); err != nil {
		return nil, err
	}

	return &data, nil
}

func getAllRepos(path string) ([]*repositoryData, error) {
	// TODO consider using go-gh
	stdout, _, err := gh("api", "--cache", defaultApiCacheTime, path)
	if err != nil {
		return nil, err
	}

	repoData := []*repositoryData{}
	err = json.Unmarshal(stdout.Bytes(), &repoData)
	if err != nil {
		return nil, err
	}

	return repoData, nil
}

func getWorkflows(repoData repositoryData, last time.Duration) ([]*workflow, error) {
	workflowsPath := fmt.Sprintf("repos/%s/actions/workflows", repoData.Name)

	// TODO consider using go-gh
	stdout, _, err := gh("api", "--cache", defaultApiCacheTime, workflowsPath, "--jq", ".workflows")
	if err != nil {
		return nil, err
	}

	type workflowsPayload struct {
		Id    int `json:"id"`
		State string
		Name  string
		URL   string `json:"url"`
	}

	p := []workflowsPayload{}
	err = json.Unmarshal(stdout.Bytes(), &p)
	if err != nil {
		return nil, err
	}

	out := []*workflow{}

	type runPayload struct {
		Id         int       `json:"id"`
		CreatedAt  time.Time `json:"created_at"`
		UpdatedAt  time.Time `json:"updated_at"`
		Status     string
		Conclusion string
		URL        string
	}

	type billablePayload struct {
		MacOs struct {
			TotalMs int `json:"total_ms"`
		} `json:"MACOS"`
		Windows struct {
			TotalMs int `json:"total_ms"`
		} `json:"WINDOWS"`
		Ubuntu struct {
			TotalMs int `json:"total_ms"`
		} `json:"UBUNTU"`
	}

	var totalMs int

	for _, w := range p {
		if strings.HasPrefix(w.State, "disabled") {
			continue
		}

		runsPath := fmt.Sprintf("%s/runs", w.URL)
		// TODO consider using go-gh
		stdout, _, err = gh("api", "--cache", defaultApiCacheTime, runsPath, "--jq", ".workflow_runs")
		if err != nil {
			return nil, fmt.Errorf("could not call gh: %w", err)
		}
		rs := []runPayload{}
		err = json.Unmarshal(stdout.Bytes(), &rs)
		if err != nil {
			return nil, fmt.Errorf("could not parse json: %w", err)
		}

		runs := []run{}

		for _, r := range rs {
			rr := run{Status: r.Status, Conclusion: r.Conclusion, URL: r.URL}

			if r.Status == "completed" {
				rr.Finished = r.UpdatedAt
				rr.Elapsed = r.UpdatedAt.Sub(r.CreatedAt)
				finishedAgo := time.Since(rr.Finished)

				if last-finishedAgo > 0 {
					runs = append(runs, rr)
				}
			}
		}

		if repoData.Private {
			for _, r := range runs {
				runTimingPath := fmt.Sprintf("%s/timing", r.URL)
				// TODO consider using go-gh
				stdout, _, err = gh("api", "--cache", defaultApiCacheTime, runTimingPath, "--jq", ".billable")
				if err != nil {
					return nil, fmt.Errorf("could not call gh: %w", err)
				}
				bp := billablePayload{}
				err = json.Unmarshal(stdout.Bytes(), &bp)
				if err != nil {
					return nil, fmt.Errorf("could not parse json: %w", err)
				}

				totalMs += bp.MacOs.TotalMs + bp.Windows.TotalMs + bp.Ubuntu.TotalMs
			}
		}

		out = append(out, &workflow{
			Name:       w.Name,
			Runs:       runs,
			BillableMs: totalMs,
		})
	}

	return out, nil
}

func parseArgs() (*options, error) {
	repositories := flag.StringSliceP("repos", "r", []string{}, "One or more repository names from the provided org or user")
	last := flag.StringP("last", "l", "30d", "What period of time to cover in hours (eg 1h) or days (eg 30d). Default: 30d")

	flag.Parse()

	if len(flag.Args()) != 1 {
		return nil, errors.New("need exactly one argument, either an organization or user name")
	}

	lastVal := *last
	timeUnit := string(lastVal[len(lastVal)-1])

	// Go cannot parse duration "1d" which is stupid; need to convert it to hours before we can get a proper duration.
	if timeUnit == "d" {
		asNum, err := strconv.Atoi(lastVal[0 : len(lastVal)-1])
		if err != nil {
			return nil, fmt.Errorf("could not parse number: %w", err)
		}
		lastVal = fmt.Sprintf("%dh", asNum*24)
	}

	if timeUnit != "h" && timeUnit != "d" {
		return nil, fmt.Errorf("report duration should be in hours or duration (eg 1h or 30d)")
	}

	duration, err := time.ParseDuration(lastVal)

	if err != nil {
		return nil, fmt.Errorf("failed to parse duration: %w", err)
	}

	return &options{
		Repositories: *repositories,
		Last:         duration,
		Selector:     flag.Arg(0),
	}, nil
}

func main() {
	opts, err := parseArgs()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to parse arguments: %s\n", err)
		os.Exit(1)
	}

	// TODO testing is annoying bc of flag.Parse() in _main
	err = _main(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		os.Exit(1)
	}
}

// gh shells out to gh, returning STDOUT/STDERR and any error
func gh(args ...string) (sout, eout bytes.Buffer, err error) {
	ghBin, err := safeexec.LookPath("gh")
	if err != nil {
		err = fmt.Errorf("could not find gh. Is it installed? error: %w", err)
		return
	}

	cmd := exec.Command(ghBin, args...)
	cmd.Stderr = &eout
	cmd.Stdout = &sout

	err = cmd.Run()
	if err != nil {
		err = fmt.Errorf("failed to run gh. error: %w, stderr: %s", err, eout.String())
		return
	}

	return
}
