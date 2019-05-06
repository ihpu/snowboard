package main

//go:generate esc -o templates.go ./templates

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/bukalapak/snowboard/adapter/drafter"
	"github.com/bukalapak/snowboard/api"
	"github.com/bukalapak/snowboard/mock"
	snowboard "github.com/bukalapak/snowboard/parser"
	"github.com/bukalapak/snowboard/render"
	"github.com/fsnotify/fsnotify"
	xerrors "github.com/pkg/errors"
	pWatcher "github.com/radovskyb/watcher"
	"github.com/rs/cors"
	cli "gopkg.in/urfave/cli.v1"
)

var (
	versionStr string
	engine     snowboard.Parser
)

func main() {
	engine = drafter.Engine{}

	cli.VersionPrinter = func(c *cli.Context) {
		fmt.Fprintf(c.App.Writer, "Snowboard version: %s\n", c.App.Version)
		fmt.Fprintf(c.App.Writer, "Drafter version: %s\n", engine.Version())
	}

	if versionStr == "" {
		versionStr = "HEAD"
	}

	app := cli.NewApp()
	app.Name = "snowboard"
	app.Usage = "API blueprint toolkit"
	app.Version = versionStr
	app.Before = func(c *cli.Context) error {
		if c.Args().Present() && c.Args().Get(1) == "" {
			cli.ShowCommandHelp(c, c.Args().Get(0))
		}

		return nil
	}
	app.Flags = []cli.Flag{
		cli.BoolFlag{
			Name:  "watch, w",
			Usage: "Watch for the files changes",
		},
		cli.StringFlag{
			Name:  "watch-interval, n",
			Usage: "Set watch interval. This activates polling watcher. Accepted format like: 100ms, 1s, etc",
		},
	}
	app.Commands = []cli.Command{
		{
			Name:  "lint",
			Usage: "Validate API blueprint",
			Action: func(c *cli.Context) error {
				if c.Args().Get(0) == "" {
					return nil
				}

				if err := validate(c, c.Args().Get(0)); err != nil {
					if strings.Contains(err.Error(), "read failed") {
						return xerrors.Cause(err)
					}

					return cli.NewExitError(err.Error(), 1)
				}

				return nil
			},
		},
		{
			Name:  "html",
			Usage: "Render HTML documentation",
			Flags: []cli.Flag{
				cli.StringFlag{
					Name:  "o",
					Usage: "HTML file",
				},
				cli.StringFlag{
					Name:  "t",
					Value: "alpha",
					Usage: "Template for HTML documentation",
				},
				cli.BoolFlag{
					Name:  "s",
					Usage: "Serve HTML via HTTP server",
				},
				cli.StringFlag{
					Name:  "b",
					Value: ":8088",
					Usage: "HTTP server listen address",
				},
				cli.BoolFlag{
					Name:  "q",
					Usage: "Quiet mode",
				},
			},
			Action: func(c *cli.Context) error {
				if c.Args().Get(0) == "" {
					return nil
				}

				if c.GlobalBool("watch") {
					cerr := make(chan error, 1)

					go func() {
						if err := renderHTML(c, c.Args().Get(0), c.String("o"), c.String("t")); err != nil {
							cerr <- cli.NewExitError(err.Error(), 1)
						}

						if c.Bool("s") {
							if err := serveHTML(c, c.String("b"), c.String("o")); err != nil {
								cerr <- cli.NewExitError(err.Error(), 1)
							}
						}

						cli.HandleExitCoder(<-cerr)
					}()

					if err := appWatcher(c); err != nil {
						return err
					}
				}

				if err := renderHTML(c, c.Args().Get(0), c.String("o"), c.String("t")); err != nil {
					return cli.NewExitError(err.Error(), 1)
				}

				if c.Bool("s") {
					if err := serveHTML(c, c.String("b"), c.String("o")); err != nil {
						return cli.NewExitError(err.Error(), 1)
					}
				}

				return nil
			},
		},
		{
			Name:  "apib",
			Usage: "Render API blueprint",
			Flags: []cli.Flag{
				cli.StringFlag{
					Name:  "o",
					Usage: "API blueprint output file",
				},
				cli.BoolFlag{
					Name:  "q",
					Usage: "Quiet mode",
				},
			},
			Action: func(c *cli.Context) error {
				if c.Args().Get(0) == "" {
					return nil
				}

				if c.GlobalBool("watch") {
					if err := appWatcher(c); err != nil {
						return err
					}
				}

				if err := renderAPIB(c, c.Args().Get(0), c.String("o")); err != nil {
					return cli.NewExitError(err.Error(), 1)
				}

				return nil
			},
		},
		{
			Name:  "json",
			Usage: "Render API element json",
			Flags: []cli.Flag{
				cli.StringFlag{
					Name:  "o",
					Usage: "API element output file",
				},
				cli.BoolFlag{
					Name:  "q",
					Usage: "Quiet mode",
				},
			},
			Action: func(c *cli.Context) error {
				if c.Args().Get(0) == "" {
					return nil
				}

				if c.GlobalBool("watch") {
					if err := appWatcher(c); err != nil {
						return err
					}
				}

				if err := renderJSON(c, c.Args().Get(0), c.String("o")); err != nil {
					return cli.NewExitError(err.Error(), 1)
				}

				return nil
			},
		},
		{
			Name:  "list",
			Usage: "List available routes",
			Action: func(c *cli.Context) error {
				if c.Args().Get(0) == "" {
					return nil
				}
				if err := outputPath(c, c.Args()); err != nil {
					return cli.NewExitError(err.Error(), 1)
				}
				return nil
			},
		},
		{
			Name:  "mock",
			Usage: "Run Mock server",
			Flags: []cli.Flag{
				cli.StringFlag{
					Name:  "b",
					Value: ":8087",
					Usage: "HTTP server listen address",
				},
			},
			Action: func(c *cli.Context) error {
				if c.Args().Get(0) == "" {
					return nil
				}

				if err := serveMock(c, c.String("b"), c.Args()); err != nil {
					return cli.NewExitError(err.Error(), 1)
				}

				return nil
			},
		},
	}

	app.Run(os.Args)
}

func appWatcher(c *cli.Context) error {
	if n := c.GlobalString("watch-interval"); n != "" {
		d, err := time.ParseDuration(n)
		if err != nil {
			return cli.NewExitError(fmt.Errorf("invalid value for `watch-interval`: %s", err), 1)
		}

		if err := watchInterval(c, c.Args().Get(0), c.String("o"), c.String("t"), d); err != nil {
			return cli.NewExitError(err.Error()+"\n", 1)
		}
	} else {
		if err := watch(c, c.Args().Get(0), c.String("o"), c.String("t")); err != nil {
			return cli.NewExitError(err.Error()+"\n", 1)
		}
	}

	return nil
}

func readFile(fn string) ([]byte, error) {
	info, err := os.Stat(fn)
	if err != nil {
		return nil, errors.New("File is not exist")
	}

	if info.IsDir() {
		return nil, errors.New("File is a directory")
	}

	return ioutil.ReadFile(fn)
}

func readTemplate(fn string) ([]byte, error) {
	tf, err := readFile(fn)
	if err == nil {
		return tf, nil
	}

	fs := FS(false)
	ff, err := fs.Open("/templates/" + fn + ".html")
	if err != nil {
		return nil, err
	}

	defer ff.Close()
	return ioutil.ReadAll(ff)
}

func renderHTML(c *cli.Context, input, output, tplFile string) error {
	bp, err := snowboard.Load(input, engine)
	if err != nil {
		return err
	}

	tf, err := readTemplate(tplFile)
	if err != nil {
		return err
	}

	if output == "" {
		var bf bytes.Buffer

		if err = render.HTML(string(tf), &bf, bp); err != nil {
			return err
		}

		fmt.Fprintln(c.App.Writer, bf.String())
		return nil
	}

	of, err := os.Create(output)
	if err != nil {
		return err
	}
	defer of.Close()

	err = render.HTML(string(tf), of, bp)
	if err != nil {
		return err
	}

	if !c.Bool("q") {
		fmt.Fprintf(c.App.Writer, "[%s] %s: HTML has been generated!\n", time.Now().Format(time.RFC3339), of.Name())
	}

	return nil
}

func renderAPIB(c *cli.Context, input, output string) error {
	b, err := snowboard.Read(input)
	if err != nil {
		return err
	}

	if output == "" {
		fmt.Fprintln(c.App.Writer, string(b))
		return nil
	}

	of, err := os.Create(output)
	if err != nil {
		return err
	}
	defer of.Close()

	_, err = io.Copy(of, bytes.NewReader(b))
	if err != nil {
		return err
	}

	if !c.Bool("q") {
		fmt.Fprintf(c.App.Writer, "%s: API blueprint has been generated!\n", of.Name())
	}

	return nil
}

func renderJSON(c *cli.Context, input, output string) error {
	b, err := snowboard.LoadAsJSON(input, engine)
	if err != nil {
		return err
	}

	if output == "" {
		fmt.Fprintln(c.App.Writer, string(b))
		return nil
	}

	of, err := os.Create(output)
	if err != nil {
		return err
	}
	defer of.Close()

	_, err = io.Copy(of, bytes.NewReader(b))
	if err != nil {
		return err
	}

	if !c.Bool("q") {
		fmt.Fprintf(c.App.Writer, "%s: API element JSON has been generated!\n", of.Name())
	}

	return nil
}

func validate(c *cli.Context, input string) error {
	b, err := snowboard.Read(input)
	if err != nil {
		return xerrors.Wrap(err, "read failed")
	}

	bf := bytes.NewReader(b)

	out, err := snowboard.Validate(bf, engine)
	if err != nil {
		return err
	}

	if out == nil {
		fmt.Fprintln(c.App.Writer, "OK")
		return nil
	}

	var buf bytes.Buffer

	s := "--------"
	w := tabwriter.NewWriter(&buf, 8, 0, 0, ' ', tabwriter.Debug)
	fmt.Fprintln(w, "Char Index\tDescription")
	fmt.Fprintf(w, "%s\t%s\n", s, strings.Repeat(s, 8))

	for _, n := range out.Annotations {
		for _, m := range n.SourceMaps {
			fmt.Fprintf(w, "%d:%d\t%s\n", m.Row, m.Col, n.Description)
		}
	}

	w.Flush()

	if len(out.Annotations) > 0 {
		return errors.New(buf.String())
	}

	return nil
}

func dash(n int) string {
	return strings.Repeat("-", n)
}

type fsWatcher interface {
	Add(string) error
}

func outputName(c *cli.Context, output string) string {
	switch c.Command.Name {
	case "html":
		if output == "" {
			return "index.html"
		} else {
			return output
		}
	}

	return ""
}

func actionCommand(c *cli.Context, input, output, tplFile string) error {
	switch c.Command.Name {
	case "html":
		if err := renderHTML(c, input, output, tplFile); err != nil {
			return err
		}
	case "apib":
		if err := renderAPIB(c, input, output); err != nil {
			return err
		}
	case "json":
		if err := renderJSON(c, input, output); err != nil {
			return err
		}
	}

	return nil
}

func watch(c *cli.Context, input, output, tplFile string) error {
	output = outputName(c, output)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	done := make(chan bool)

	go func() {
		for {
			select {
			case event := <-watcher.Events:
				if event.Op&fsnotify.Write == fsnotify.Write {
					if err := actionCommand(c, input, output, tplFile); err != nil {
						fmt.Fprintln(c.App.Writer, err)
					}
				}
			case err := <-watcher.Errors:
				fmt.Fprintln(c.App.Writer, err)
			}
		}
	}()

	if err := watchFiles(c, watcher, input, tplFile); err != nil {
		return err
	}

	<-done

	return nil
}

func watchInterval(c *cli.Context, input, output, tplFile string, interval time.Duration) error {
	output = outputName(c, output)

	watcher := pWatcher.New()
	defer watcher.Close()

	go func() {
		for {
			select {
			case event := <-watcher.Event:
				if event.Op&pWatcher.Write == pWatcher.Write {
					if err := actionCommand(c, input, output, tplFile); err != nil {
						fmt.Fprintln(c.App.Writer, err)
					}
				}
			case err := <-watcher.Error:
				fmt.Fprintln(c.App.Writer, err)
			case <-watcher.Closed:
				return
			}
		}
	}()

	if err := watchFiles(c, watcher, input, tplFile); err != nil {
		return err
	}

	if err := watcher.Start(interval); err != nil {
		return err
	}

	return nil
}

func watchFiles(c *cli.Context, watcher fsWatcher, input, tplFile string) error {
	if err := watcher.Add(input); err != nil {
		return err
	}

	if tplFile != "" {
		if _, err := os.Stat(tplFile); err == nil {
			if err = watcher.Add(tplFile); err != nil {
				return err
			}
		}
	}

	for _, s := range snowboard.Seeds(input) {
		if err := watcher.Add(s); err != nil {
			return err
		}
	}

	return nil
}

func outputPath(c *cli.Context, inputs []string) error {
	bs := make([]*api.API, len(inputs))
	for i := range inputs {
		bp, err := snowboard.Load(inputs[i], engine)
		if err != nil {
			return err
		}

		bs[i] = bp
	}
	ms := mock.MockMulti(bs)
	for _, mm := range ms {
		for _, m := range mm {
			fmt.Fprintf(c.App.Writer, "%s\t%d\t%s\n", m.Method, m.StatusCode, m.Pattern)
		}
	}
	return nil
}

func serveHTML(c *cli.Context, bind, output string) error {
	fmt.Fprintf(c.App.Writer, "snowboard: listening on %s\n", bind)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, output)
	})

	return http.ListenAndServe(bind, nil)
}

func serveMock(c *cli.Context, bind string, inputs []string) error {
	bs := make([]*api.API, len(inputs))

	for i := range inputs {
		bp, err := snowboard.Load(inputs[i], engine)
		if err != nil {
			return err
		}

		bs[i] = bp
	}

	fmt.Fprintf(c.App.Writer, "Mock server is ready. Use %s\n", bind)
	fmt.Fprintln(c.App.Writer, "Available Routes:")

	ms := mock.MockMulti(bs)
	for _, mm := range ms {
		for _, m := range mm {
			fmt.Fprintf(c.App.Writer, "%s\t%d\t%s\n", m.Method, m.StatusCode, m.Pattern)
		}
	}

	h := mock.MockHandler(ms)
	z := cors.AllowAll().Handler(h)

	return http.ListenAndServe(bind, z)
}
