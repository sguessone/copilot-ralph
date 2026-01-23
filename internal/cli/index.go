package cli

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/sguessone/copilot-ralph/internal/indexer"
)

var (
	indexRoot       string
	indexSave       string
	searchIndexPath string
	searchQuery     string
	searchK         int
)

func init() {
	rootCmd.AddCommand(indexCmd)
	rootCmd.AddCommand(searchCmd)

	indexCmd.Flags().StringVar(&indexRoot, "root", ".", "root directory to index")
	indexCmd.Flags().StringVar(&indexSave, "save", ".repo_index.json", "path to save index JSON")

	searchCmd.Flags().StringVar(&searchIndexPath, "index", ".repo_index.json", "index file to load")
	searchCmd.Flags().StringVar(&searchQuery, "q", "", "query string")
	searchCmd.Flags().IntVar(&searchK, "k", 5, "number of results to return")
}

var indexCmd = &cobra.Command{
	Use:   "index",
	Short: "Index repository files and save index",
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := filepath.Abs(indexRoot)
		if err != nil {
			return err
		}

		idx, err := indexer.IndexRepo(root)
		if err != nil {
			return err
		}

		savePath := indexSave
		if !filepath.IsAbs(savePath) {
			savePath = filepath.Join(root, savePath)
		}

		if err := idx.Save(savePath); err != nil {
			return err
		}

		fmt.Printf("Indexed %d chunks and saved to %s\n", len(idx.Chunks), savePath)
		return nil
	},
}

var searchCmd = &cobra.Command{
	Use:   "search",
	Short: "Search an index for relevant snippets",
	RunE: func(cmd *cobra.Command, args []string) error {
		if searchQuery == "" {
			return fmt.Errorf("query must be provided via --q")
		}

		idx, err := indexer.Load(searchIndexPath)
		if err != nil {
			// try to index current dir
			idx2, err2 := indexer.IndexRepo(".")
			if err2 != nil {
				return err
			}
			idx = idx2
		}

		results := idx.Search(searchQuery, searchK)
		if len(results) == 0 {
			fmt.Println("no results")
			return nil
		}

		for i, r := range results {
			fmt.Printf("%d) [score=%.4f] %s\n", i+1, r.Score, r.Path)
			fmt.Println(r.Text)
			fmt.Println("---")
		}

		return nil
	},
}
