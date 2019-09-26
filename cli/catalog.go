package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strings"

	"github.com/Masterminds/semver"

	"github.com/digitalrebar/provision/v4/models"
	"github.com/spf13/cobra"
)

func getLocalCatalog() (res *models.Content, err error) {
	req := Session.Req().List("files")
	req.Params("path", "rebar-catalog/rackn-catalog")
	data := []interface{}{}
	err = req.Do(&data)
	if err != nil {
		return
	}

	if len(data) == 0 {
		err = fmt.Errorf("Failed to find local catalog")
		return
	}

	vs := make([]*semver.Version, len(data))
	vmap := map[string]string{}
	for i, obj := range data {
		r := obj.(string)
		v, verr := semver.NewVersion(strings.TrimSuffix(r, ".json"))
		if verr != nil {
			err = verr
			return
		}
		vs[i] = v
		vmap[v.String()] = r
	}
	sort.Sort(sort.Reverse(semver.Collection(vs)))

	var buf bytes.Buffer
	path := fmt.Sprintf("rebar-catalog/rackn-catalog/%s", vmap[vs[0].String()])
	fmt.Printf("Using catalog: %s\n", path)
	if gerr := Session.GetBlob(&buf, "files", path); gerr != nil {
		err = fmt.Errorf("Failed to fetch %v: %v: %v", "files", path, gerr)
		return
	}

	err = json.Unmarshal(buf.Bytes(), &res)
	return
}

func catalogCommands() *cobra.Command {

	type catItem struct {
		Type     string
		Versions []string
	}

	fetchCatalog := func() (res *models.Content, err error) {
		buf := []byte{}
		buf, err = bufOrFile(catalog)
		if err == nil {
			err = json.Unmarshal(buf, &res)
		}
		if err != nil {
			err = fmt.Errorf("Error fetching catalog: %v", err)
		}
		return
	}

	itemsFromCatalog := func(cat *models.Content, name string) map[string]*models.CatalogItem {
		res := map[string]*models.CatalogItem{}
		for k, v := range cat.Sections["catalog_items"] {
			item := &models.CatalogItem{}
			if err := models.Remarshal(v, &item); err != nil {
				continue
			}
			if name == "" || name == item.Name {
				res[k] = item
			}
		}
		return res
	}

	oneItem := func(cat *models.Content, name, version string) *models.CatalogItem {
		items := itemsFromCatalog(cat, name)
		for _, v := range items {
			if v.Version == version {
				return v
			}
		}
		return nil
	}

	cmd := &cobra.Command{
		Use:   "catalog",
		Short: "Access commands related to catalog manipulation",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Show the contents of the current catalog",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			catalog, err := fetchCatalog()
			if err != nil {
				return err
			}
			return prettyPrint(catalog)
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "items",
		Short: "Show the items available in the catalog",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			catalog, err := fetchCatalog()
			if err != nil {
				return err
			}

			items := map[string]catItem{}
			for _, v := range itemsFromCatalog(catalog, "") {
				item := &models.CatalogItem{}
				if err := models.Remarshal(v, &item); err != nil {
					continue
				}
				if _, ok := items[item.Name]; !ok {
					items[item.Name] = catItem{Type: item.ContentType, Versions: []string{item.Version}}
				} else {
					cat := items[item.Name]
					cat.Versions = append(cat.Versions, item.Version)
					items[item.Name] = cat
				}
			}
			for k := range items {
				sort.Strings(items[k].Versions)
			}
			return prettyPrint(items)
		},
	})
	itemCmd := &cobra.Command{
		Use:   "item",
		Short: "Commands to act on individual catalog items",
	}
	var arch, tgtos, version string
	itemCmd.PersistentFlags().StringVar(&arch, "arch", runtime.GOARCH, "Architecture of the item to work with when downloading a plugin provider")
	itemCmd.PersistentFlags().StringVar(&tgtos, "os", runtime.GOOS, "OS of the item to work with when downloading a plugin provider")
	itemCmd.PersistentFlags().StringVar(&version, "version", "stable", "Version of the item to work with")
	itemCmd.AddCommand(&cobra.Command{
		Use:   "download [item] (to [file])",
		Short: "Downloads [item] to [file]",
		Long: `Downloads the specified item to the specified file
If to [file] is not specified, it will be downloaded into current directory
and wind up in a file with the same name as the item + the default file extension for the file type.
`,
		Args: func(c *cobra.Command, args []string) error {
			if len(args) != 1 && len(args) != 3 {
				return fmt.Errorf("item download requires 1 or 2 arguments")
			}
			return nil
		},
		RunE: func(c *cobra.Command, args []string) error {
			catalog, err := fetchCatalog()
			if err != nil {
				return err
			}
			item := oneItem(catalog, args[0], version)
			if item == nil {
				return fmt.Errorf("%s version %s not in catalog", args[0], version)
			}
			target := item.FileName()
			if len(args) == 3 {
				target = args[2]
			}
			mode := os.FileMode(0644)
			if item.ContentType == "PluginProvider" {
				mode = 0755
			}
			src, err := urlOrFileAsReadCloser(item.DownloadUrl(arch, tgtos))
			if src != nil {
				defer src.Close()
			}
			if err != nil {
				return fmt.Errorf("Unable to contact source URL for %s: %v", item.Name, err)
			}
			fi, err := os.OpenFile(target, os.O_RDWR|os.O_CREATE|os.O_TRUNC, mode)
			if err != nil {
				return fmt.Errorf("Unable to create %s: %v", target, err)
			}
			defer fi.Close()
			_, err = io.Copy(fi, src)
			return err
		},
	})
	itemCmd.AddCommand(&cobra.Command{
		Use:               "install [item]",
		Short:             "Installs [item] from the catalog on the current dr-provision endpoint",
		PersistentPreRunE: ppr,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("item install requires 1 argument")
			}
			return nil
		},
		RunE: func(c *cobra.Command, args []string) error {
			catalog, err := fetchCatalog()
			if err != nil {
				return err
			}
			info, err := Session.Info()
			if err != nil {
				return fmt.Errorf("Unable to fetch session information to determine endpoint arch and OS")
			}
			item := oneItem(catalog, args[0], version)
			if item == nil {
				return fmt.Errorf("%s version %s not in catalog", args[0], version)
			}
			arch = info.Arch
			tgtos = info.Os
			src, err := urlOrFileAsReadCloser(item.DownloadUrl(arch, tgtos))
			if src != nil {
				defer src.Close()
			}
			if err != nil {
				return fmt.Errorf("Unable to contact source URL for %s: %v", item.Name, err)
			}
			switch item.ContentType {
			case "ContentPackage":
				content := &models.Content{}
				if err := json.NewDecoder(src).Decode(&content); err != nil {
					return fmt.Errorf("Error downloading content bundle %s: %v", item.Name, err)
				}
				return doReplaceContent(content, "")
			case "PluginProvider":
				res := &models.PluginProviderUploadInfo{}
				if err := Session.Req().Post(src).UrlFor("plugin_providers", item.Name).Do(res); err != nil {
					return err
				}
				return prettyPrint(res)
			default:
				return fmt.Errorf("Don't know how to install %s of type %s yet", item.Name, item.ContentType)
			}
		},
	})
	itemCmd.AddCommand(&cobra.Command{
		Use:   "show [item]",
		Short: "Shows available versions for [item]",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("item show requires 1 argument")
			}
			return nil
		},
		RunE: func(c *cobra.Command, args []string) error {
			catalog, err := fetchCatalog()
			if err != nil {
				return err
			}

			items := map[string]catItem{}
			for _, v := range itemsFromCatalog(catalog, args[0]) {
				item := &models.CatalogItem{}
				if err := models.Remarshal(v, &item); err != nil {
					continue
				}
				if _, ok := items[item.Name]; !ok {
					items[item.Name] = catItem{Type: item.ContentType, Versions: []string{item.Version}}
				} else {
					cat := items[item.Name]
					cat.Versions = append(cat.Versions, item.Version)
					items[item.Name] = cat
				}
			}
			if len(items) == 0 {
				return fmt.Errorf("No item named %s in the catalog", args[0])
			}
			for k := range items {
				sort.Strings(items[k].Versions)
			}
			return prettyPrint(items[args[0]])
		},
	})
	cmd.AddCommand(itemCmd)

	updateCmd := &cobra.Command{
		Use:   "updateLocal",
		Short: "Update the local catalog from the upstream catalog",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			srcCatalog, err := fetchCatalog()
			if err != nil {
				return err
			}

			localCatalog, err := getLocalCatalog()
			if err != nil {
				return err
			}

			srcItems := itemsFromCatalog(srcCatalog, "")
			localItems := itemsFromCatalog(localCatalog, "")

			for k, v := range srcItems {
				// Only get things that aren't tip or stable
				if strings.HasSuffix(k, "-tip") || strings.HasSuffix(k, "-stable") {
					continue
				}

				// Get things that aren't in local
				if _, ok := localItems[k]; !ok {
					parts := map[string]string{}
					i := strings.Index(v.Source, "/rebar-catalog/")
					switch v.ContentType {
					case "PluginProvider", "DRPCLI":
						for arch := range v.Shasum256 {
							ts := fmt.Sprintf("%s/%s/%s", v.Source, arch, v.Name)
							qs, _ := url.QueryUnescape(v.Source[i+1:])
							td := fmt.Sprintf("%s/%s/%s", qs, arch, v.Name)
							parts[ts] = td
						}
					default:
						parts[v.Source], _ = url.QueryUnescape(v.Source[i+1:])
					}

					for s, d := range parts {
						fmt.Printf("Downloading %s to store at %s\n", s, d)
						data, err := urlOrFileAsReadCloser(s)
						if err != nil {
							return fmt.Errorf("Error opening src file %s: %v", s, err)
						}
						defer data.Close()
						if _, err := Session.PostBlobExplode(data, false, "files", d); err != nil {
							return generateError(err, "Failed to post %v: %v", "files", d)
						}
					}
				}
			}
			return nil
		},
	}
	cmd.AddCommand(updateCmd)

	return cmd
}

func init() {
	addRegistrar(func(c *cobra.Command) { c.AddCommand(catalogCommands()) })
}