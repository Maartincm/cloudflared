package tunnel

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	homedir "github.com/mitchellh/go-homedir"
	"github.com/pkg/errors"
	"github.com/urfave/cli/v2"
	"github.com/urfave/cli/v2/altsrc"
	"golang.org/x/net/idna"
	yaml "gopkg.in/yaml.v2"

	"github.com/cloudflare/cloudflared/cmd/cloudflared/cliutil"
	"github.com/cloudflare/cloudflared/cmd/cloudflared/updater"
	"github.com/cloudflare/cloudflared/config"
	"github.com/cloudflare/cloudflared/connection"
	"github.com/cloudflare/cloudflared/tunnelstore"
)

const (
	allSortByOptions     = "name, id, createdAt, deletedAt, numConnections"
	connsSortByOptions   = "id, startedAt, numConnections, version"
	CredFileFlagAlias    = "cred-file"
	CredFileFlag         = "credentials-file"
	overwriteDNSFlagName = "overwrite-dns"

	LogFieldTunnelID = "tunnelID"
)

var (
	showDeletedFlag = &cli.BoolFlag{
		Name:    "show-deleted",
		Aliases: []string{"d"},
		Usage:   "Include deleted tunnels in the list",
	}
	listNameFlag = &cli.StringFlag{
		Name:    "name",
		Aliases: []string{"n"},
		Usage:   "List tunnels with the given `NAME`",
	}
	listExistedAtFlag = &cli.TimestampFlag{
		Name:        "when",
		Aliases:     []string{"w"},
		Usage:       "List tunnels that are active at the given `TIME` in RFC3339 format",
		Layout:      tunnelstore.TimeLayout,
		DefaultText: fmt.Sprintf("current time, %s", time.Now().Format(tunnelstore.TimeLayout)),
	}
	listIDFlag = &cli.StringFlag{
		Name:    "id",
		Aliases: []string{"i"},
		Usage:   "List tunnel by `ID`",
	}
	showRecentlyDisconnected = &cli.BoolFlag{
		Name:    "show-recently-disconnected",
		Aliases: []string{"rd"},
		Usage:   "Include connections that have recently disconnected in the list",
	}
	outputFormatFlag = &cli.StringFlag{
		Name:    "output",
		Aliases: []string{"o"},
		Usage:   "Render output using given `FORMAT`. Valid options are 'json' or 'yaml'",
	}
	sortByFlag = &cli.StringFlag{
		Name:    "sort-by",
		Value:   "name",
		Usage:   fmt.Sprintf("Sorts the list of tunnels by the given field. Valid options are {%s}", allSortByOptions),
		EnvVars: []string{"TUNNEL_LIST_SORT_BY"},
	}
	invertSortFlag = &cli.BoolFlag{
		Name:    "invert-sort",
		Usage:   "Inverts the sort order of the tunnel list.",
		EnvVars: []string{"TUNNEL_LIST_INVERT_SORT"},
	}
	forceFlag = altsrc.NewBoolFlag(&cli.BoolFlag{
		Name:    "force",
		Aliases: []string{"f"},
		Usage: "By default, if a tunnel is currently being run from a cloudflared, you can't " +
			"simultaneously rerun it again from a second cloudflared. The --force flag lets you " +
			"overwrite the previous tunnel. If you want to use a single hostname with multiple " +
			"tunnels, you can do so with Cloudflare's Load Balancer product.",
	})
	featuresFlag = altsrc.NewStringSliceFlag(&cli.StringSliceFlag{
		Name:    "features",
		Aliases: []string{"F"},
		Usage:   "Opt into various features that are still being developed or tested.",
	})
	credentialsFileFlagCLIOnly = &cli.StringFlag{
		Name:    CredFileFlag,
		Aliases: []string{CredFileFlagAlias},
		Usage:   "Filepath at which to read/write the tunnel credentials",
		EnvVars: []string{"TUNNEL_CRED_FILE"},
	}
	credentialsFileFlag = altsrc.NewStringFlag(credentialsFileFlagCLIOnly)
	forceDeleteFlag     = &cli.BoolFlag{
		Name:    "force",
		Aliases: []string{"f"},
		Usage: "Cleans up any stale connections before the tunnel is deleted. cloudflared will not " +
			"delete a tunnel with connections without this flag.",
		EnvVars: []string{"TUNNEL_RUN_FORCE_OVERWRITE"},
	}
	selectProtocolFlag = altsrc.NewStringFlag(&cli.StringFlag{
		Name:    "protocol",
		Value:   "auto",
		Aliases: []string{"p"},
		Usage:   fmt.Sprintf("Protocol implementation to connect with Cloudflare's edge network. %s", connection.AvailableProtocolFlagMessage),
		EnvVars: []string{"TUNNEL_TRANSPORT_PROTOCOL"},
		Hidden:  true,
	})
	sortInfoByFlag = &cli.StringFlag{
		Name:    "sort-by",
		Value:   "createdAt",
		Usage:   fmt.Sprintf("Sorts the list of connections of a tunnel by the given field. Valid options are {%s}", connsSortByOptions),
		EnvVars: []string{"TUNNEL_INFO_SORT_BY"},
	}
	invertInfoSortFlag = &cli.BoolFlag{
		Name:    "invert-sort",
		Usage:   "Inverts the sort order of the tunnel info.",
		EnvVars: []string{"TUNNEL_INFO_INVERT_SORT"},
	}
	cleanupClientFlag = &cli.StringFlag{
		Name:    "connector-id",
		Aliases: []string{"c"},
		Usage:   `Constraints the cleanup to stop the connections of a single Connector (by its ID). You can find the various Connectors (and their IDs) currently connected to your tunnel via 'cloudflared tunnel info <name>'.`,
		EnvVars: []string{"TUNNEL_CLEANUP_CONNECTOR"},
	}
	overwriteDNSFlag = &cli.BoolFlag{
		Name:    overwriteDNSFlagName,
		Aliases: []string{"f"},
		Usage:   `Overwrites existing DNS records with this hostname`,
		EnvVars: []string{"TUNNEL_FORCE_PROVISIONING_DNS"},
	}
)

func buildCreateCommand() *cli.Command {
	return &cli.Command{
		Name:      "create",
		Action:    cliutil.ConfiguredAction(createCommand),
		Usage:     "Create a new tunnel with given name",
		UsageText: "cloudflared tunnel [tunnel command options] create [subcommand options] NAME",
		Description: `Creates a tunnel, registers it with Cloudflare edge and generates credential file used to run this tunnel.
  Use "cloudflared tunnel route" subcommand to map a DNS name to this tunnel and "cloudflared tunnel run" to start the connection.

  For example, to create a tunnel named 'my-tunnel' run:

  $ cloudflared tunnel create my-tunnel`,
		Flags:              []cli.Flag{outputFormatFlag, credentialsFileFlagCLIOnly},
		CustomHelpTemplate: commandHelpTemplate(),
	}
}

// generateTunnelSecret as an array of 32 bytes using secure random number generator
func generateTunnelSecret() ([]byte, error) {
	randomBytes := make([]byte, 32)
	_, err := rand.Read(randomBytes)
	return randomBytes, err
}

func createCommand(c *cli.Context) error {
	sc, err := newSubcommandContext(c)
	if err != nil {
		return errors.Wrap(err, "error setting up logger")
	}

	if c.NArg() != 1 {
		return cliutil.UsageError(`"cloudflared tunnel create" requires exactly 1 argument, the name of tunnel to create.`)
	}
	name := c.Args().First()

	warningChecker := updater.StartWarningCheck(c)
	defer warningChecker.LogWarningIfAny(sc.log)

	_, err = sc.create(name, c.String(CredFileFlag))
	return errors.Wrap(err, "failed to create tunnel")
}

func tunnelFilePath(tunnelID uuid.UUID, directory string) (string, error) {
	fileName := fmt.Sprintf("%v.json", tunnelID)
	filePath := filepath.Clean(fmt.Sprintf("%s/%s", directory, fileName))
	return homedir.Expand(filePath)
}

// writeTunnelCredentials saves `credentials` as a JSON into `filePath`, only if
// the file does not exist already
func writeTunnelCredentials(filePath string, credentials *connection.Credentials) error {
	if _, err := os.Stat(filePath); !os.IsNotExist(err) {
		if err == nil {
			return fmt.Errorf("%s already exists", filePath)
		}
		return err
	}
	body, err := json.Marshal(credentials)
	if err != nil {
		return errors.Wrap(err, "Unable to marshal tunnel credentials to JSON")
	}
	return ioutil.WriteFile(filePath, body, 400)
}

func buildListCommand() *cli.Command {
	return &cli.Command{
		Name:        "list",
		Action:      cliutil.ConfiguredAction(listCommand),
		Usage:       "List existing tunnels",
		UsageText:   "cloudflared tunnel [tunnel command options] list [subcommand options]",
		Description: "cloudflared tunnel list will display all active tunnels, their created time and associated connections. Use -d flag to include deleted tunnels. See the list of options to filter the list",
		Flags: []cli.Flag{
			outputFormatFlag,
			showDeletedFlag,
			listNameFlag,
			listExistedAtFlag,
			listIDFlag,
			showRecentlyDisconnected,
			sortByFlag,
			invertSortFlag,
		},
		CustomHelpTemplate: commandHelpTemplate(),
	}
}

func listCommand(c *cli.Context) error {
	sc, err := newSubcommandContext(c)
	if err != nil {
		return err
	}

	warningChecker := updater.StartWarningCheck(c)
	defer warningChecker.LogWarningIfAny(sc.log)

	filter := tunnelstore.NewFilter()
	if !c.Bool("show-deleted") {
		filter.NoDeleted()
	}
	if name := c.String("name"); name != "" {
		filter.ByName(name)
	}
	if existedAt := c.Timestamp("time"); existedAt != nil {
		filter.ByExistedAt(*existedAt)
	}
	if id := c.String("id"); id != "" {
		tunnelID, err := uuid.Parse(id)
		if err != nil {
			return errors.Wrapf(err, "%s is not a valid tunnel ID", id)
		}
		filter.ByTunnelID(tunnelID)
	}

	tunnels, err := sc.list(filter)
	if err != nil {
		return err
	}

	// Sort the tunnels
	sortBy := c.String("sort-by")
	invalidSortField := false
	sort.Slice(tunnels, func(i, j int) bool {
		cmp := func() bool {
			switch sortBy {
			case "name":
				return tunnels[i].Name < tunnels[j].Name
			case "id":
				return tunnels[i].ID.String() < tunnels[j].ID.String()
			case "createdAt":
				return tunnels[i].CreatedAt.Unix() < tunnels[j].CreatedAt.Unix()
			case "deletedAt":
				return tunnels[i].DeletedAt.Unix() < tunnels[j].DeletedAt.Unix()
			case "numConnections":
				return len(tunnels[i].Connections) < len(tunnels[j].Connections)
			default:
				invalidSortField = true
				return tunnels[i].Name < tunnels[j].Name
			}
		}()
		if c.Bool("invert-sort") {
			return !cmp
		}
		return cmp
	})
	if invalidSortField {
		sc.log.Error().Msgf("%s is not a valid sort field. Valid sort fields are %s. Defaulting to 'name'.", sortBy, allSortByOptions)
	}

	if outputFormat := c.String(outputFormatFlag.Name); outputFormat != "" {
		return renderOutput(outputFormat, tunnels)
	}

	if len(tunnels) > 0 {
		formatAndPrintTunnelList(tunnels, c.Bool("show-recently-disconnected"))
	} else {
		fmt.Println("You have no tunnels, use 'cloudflared tunnel create' to define a new tunnel")
	}

	return nil
}

func formatAndPrintTunnelList(tunnels []*tunnelstore.Tunnel, showRecentlyDisconnected bool) {
	writer := tabWriter()
	defer writer.Flush()

	_, _ = fmt.Fprintln(writer, "You can obtain more detailed information for each tunnel with `cloudflared tunnel info <name/uuid>`")

	// Print column headers with tabbed columns
	_, _ = fmt.Fprintln(writer, "ID\tNAME\tCREATED\tCONNECTIONS\t")

	// Loop through tunnels, create formatted string for each, and print using tabwriter
	for _, t := range tunnels {
		formattedStr := fmt.Sprintf(
			"%s\t%s\t%s\t%s\t",
			t.ID,
			t.Name,
			t.CreatedAt.Format(time.RFC3339),
			fmtConnections(t.Connections, showRecentlyDisconnected),
		)
		_, _ = fmt.Fprintln(writer, formattedStr)
	}
}

func fmtConnections(connections []tunnelstore.Connection, showRecentlyDisconnected bool) string {

	// Count connections per colo
	numConnsPerColo := make(map[string]uint, len(connections))
	for _, connection := range connections {
		if !connection.IsPendingReconnect || showRecentlyDisconnected {
			numConnsPerColo[connection.ColoName]++
		}
	}

	// Get sorted list of colos
	sortedColos := []string{}
	for coloName := range numConnsPerColo {
		sortedColos = append(sortedColos, coloName)
	}
	sort.Strings(sortedColos)

	// Map each colo to its frequency, combine into output string.
	var output []string
	for _, coloName := range sortedColos {
		output = append(output, fmt.Sprintf("%dx%s", numConnsPerColo[coloName], coloName))
	}
	return strings.Join(output, ", ")
}

func buildInfoCommand() *cli.Command {
	return &cli.Command{
		Name:        "info",
		Action:      cliutil.ConfiguredAction(tunnelInfo),
		Usage:       "List details about the active connectors for a tunnel",
		UsageText:   "cloudflared tunnel [tunnel command options] info [subcommand options] [TUNNEL]",
		Description: "cloudflared tunnel info displays details about the active connectors for a given tunnel (identified by name or uuid).",
		Flags: []cli.Flag{
			outputFormatFlag,
			showRecentlyDisconnected,
			sortInfoByFlag,
			invertInfoSortFlag,
		},
		CustomHelpTemplate: commandHelpTemplate(),
	}
}

func tunnelInfo(c *cli.Context) error {
	sc, err := newSubcommandContext(c)
	if err != nil {
		return err
	}

	warningChecker := updater.StartWarningCheck(c)
	defer warningChecker.LogWarningIfAny(sc.log)

	if c.NArg() != 1 {
		return cliutil.UsageError(`"cloudflared tunnel info" accepts exactly one argument, the ID or name of the tunnel to get info about.`)
	}
	tunnelID, err := sc.findID(c.Args().First())
	if err != nil {
		return errors.Wrap(err, "error parsing tunnel ID")
	}

	client, err := sc.client()
	if err != nil {
		return err
	}

	clients, err := client.ListActiveClients(tunnelID)
	if err != nil {
		return err
	}

	sortBy := c.String("sort-by")
	invalidSortField := false
	sort.Slice(clients, func(i, j int) bool {
		cmp := func() bool {
			switch sortBy {
			case "id":
				return clients[i].ID.String() < clients[j].ID.String()
			case "createdAt":
				return clients[i].RunAt.Unix() < clients[j].RunAt.Unix()
			case "numConnections":
				return len(clients[i].Connections) < len(clients[j].Connections)
			case "version":
				return clients[i].Version < clients[j].Version
			default:
				invalidSortField = true
				return clients[i].RunAt.Unix() < clients[j].RunAt.Unix()
			}
		}()
		if c.Bool("invert-sort") {
			return !cmp
		}
		return cmp
	})
	if invalidSortField {
		sc.log.Error().Msgf("%s is not a valid sort field. Valid sort fields are %s. Defaulting to 'name'.", sortBy, connsSortByOptions)
	}

	tunnel, err := getTunnel(sc, tunnelID)
	if err != nil {
		return err
	}
	info := Info{
		tunnel.ID,
		tunnel.Name,
		tunnel.CreatedAt,
		clients,
	}

	if outputFormat := c.String(outputFormatFlag.Name); outputFormat != "" {
		return renderOutput(outputFormat, info)
	}

	if len(clients) > 0 {
		formatAndPrintConnectionsList(info, c.Bool("show-recently-disconnected"))
	} else {
		fmt.Printf("Your tunnel %s does not have any active connection.\n", tunnelID)
	}

	return nil
}

func getTunnel(sc *subcommandContext, tunnelID uuid.UUID) (*tunnelstore.Tunnel, error) {
	filter := tunnelstore.NewFilter()
	filter.ByTunnelID(tunnelID)
	tunnels, err := sc.list(filter)
	if err != nil {
		return nil, err
	}
	if len(tunnels) != 1 {
		return nil, errors.Errorf("Expected to find a single tunnel with uuid %v but found %d tunnels.", tunnelID, len(tunnels))
	}
	return tunnels[0], nil
}

func formatAndPrintConnectionsList(tunnelInfo Info, showRecentlyDisconnected bool) {
	writer := tabWriter()
	defer writer.Flush()

	// Print the general tunnel info table
	_, _ = fmt.Fprintf(writer, "NAME:     %s\nID:       %s\nCREATED:  %s\n\n", tunnelInfo.Name, tunnelInfo.ID, tunnelInfo.CreatedAt)

	// Determine whether to print the connector table
	shouldDisplayTable := false
	for _, c := range tunnelInfo.Connectors {
		conns := fmtConnections(c.Connections, showRecentlyDisconnected)
		if len(conns) > 0 {
			shouldDisplayTable = true
		}
	}
	if !shouldDisplayTable {
		fmt.Println("This tunnel has no active connectors.")
		return
	}

	// Print the connector table
	_, _ = fmt.Fprintln(writer, "CONNECTOR ID\tCREATED\tARCHITECTURE\tVERSION\tORIGIN IP\tEDGE\t")
	for _, c := range tunnelInfo.Connectors {
		conns := fmtConnections(c.Connections, showRecentlyDisconnected)
		if len(conns) == 0 {
			continue
		}
		originIp := c.Connections[0].OriginIP.String()
		formattedStr := fmt.Sprintf(
			"%s\t%s\t%s\t%s\t%s\t%s\t",
			c.ID,
			c.RunAt.Format(time.RFC3339),
			c.Arch,
			c.Version,
			originIp,
			conns,
		)
		_, _ = fmt.Fprintln(writer, formattedStr)
	}
}

func tabWriter() *tabwriter.Writer {
	const (
		minWidth = 0
		tabWidth = 8
		padding  = 1
		padChar  = ' '
		flags    = 0
	)

	writer := tabwriter.NewWriter(os.Stdout, minWidth, tabWidth, padding, padChar, flags)
	return writer
}

func buildDeleteCommand() *cli.Command {
	return &cli.Command{
		Name:               "delete",
		Action:             cliutil.ConfiguredAction(deleteCommand),
		Usage:              "Delete existing tunnel by UUID or name",
		UsageText:          "cloudflared tunnel [tunnel command options] delete [subcommand options] TUNNEL",
		Description:        "cloudflared tunnel delete will delete tunnels with the given tunnel UUIDs or names. A tunnel cannot be deleted if it has active connections. To delete the tunnel unconditionally, use -f flag.",
		Flags:              []cli.Flag{credentialsFileFlagCLIOnly, forceDeleteFlag},
		CustomHelpTemplate: commandHelpTemplate(),
	}
}

func deleteCommand(c *cli.Context) error {
	sc, err := newSubcommandContext(c)
	if err != nil {
		return err
	}

	if c.NArg() < 1 {
		return cliutil.UsageError(`"cloudflared tunnel delete" requires at least 1 argument, the ID or name of the tunnel to delete.`)
	}

	warningChecker := updater.StartWarningCheck(c)
	defer warningChecker.LogWarningIfAny(sc.log)

	tunnelIDs, err := sc.findIDs(c.Args().Slice())
	if err != nil {
		return err
	}

	return sc.delete(tunnelIDs)
}

func renderOutput(format string, v interface{}) error {
	switch format {
	case "json":
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(v)
	case "yaml":
		return yaml.NewEncoder(os.Stdout).Encode(v)
	default:
		return errors.Errorf("Unknown output format '%s'", format)
	}
}

func buildRunCommand() *cli.Command {
	flags := []cli.Flag{
		forceFlag,
		credentialsFileFlag,
		selectProtocolFlag,
		featuresFlag,
	}
	flags = append(flags, configureProxyFlags(false)...)
	return &cli.Command{
		Name:      "run",
		Action:    cliutil.ConfiguredAction(runCommand),
		Usage:     "Proxy a local web server by running the given tunnel",
		UsageText: "cloudflared tunnel [tunnel command options] run [subcommand options] [TUNNEL]",
		Description: `Runs the tunnel identified by name or UUUD, creating highly available connections
  between your server and the Cloudflare edge. You can provide name or UUID of tunnel to run either as the
  last command line argument or in the configuration file using "tunnel: TUNNEL".

  This command requires the tunnel credentials file created when "cloudflared tunnel create" was run,
  however it does not need access to cert.pem from "cloudflared login" if you identify the tunnel by UUID.
  If you experience other problems running the tunnel, "cloudflared tunnel cleanup" may help by removing
  any old connection records.
`,
		Flags:              flags,
		CustomHelpTemplate: commandHelpTemplate(),
	}
}

func runCommand(c *cli.Context) error {
	sc, err := newSubcommandContext(c)
	if err != nil {
		return err
	}

	if c.NArg() > 1 {
		return cliutil.UsageError(`"cloudflared tunnel run" accepts only one argument, the ID or name of the tunnel to run.`)
	}
	tunnelRef := c.Args().First()
	if tunnelRef == "" {
		// see if tunnel id was in the config file
		tunnelRef = config.GetConfiguration().TunnelID
		if tunnelRef == "" {
			return cliutil.UsageError(`"cloudflared tunnel run" requires the ID or name of the tunnel to run as the last command line argument or in the configuration file.`)
		}
	}

	if c.String("hostname") != "" {
		sc.log.Warn().Msg("The property `hostname` in your configuration is ignored because you configured a Named Tunnel " +
			"in the property `tunnel` to run. Make sure to provision the routing (e.g. via `cloudflared tunnel route dns/lb`) or else " +
			"your origin will not be reachable. You should remove the `hostname` property to avoid this warning.")
	}

	return runNamedTunnel(sc, tunnelRef)
}

func runNamedTunnel(sc *subcommandContext, tunnelRef string) error {
	tunnelID, err := sc.findID(tunnelRef)
	if err != nil {
		return errors.Wrap(err, "error parsing tunnel ID")
	}

	sc.log.Info().Str(LogFieldTunnelID, tunnelID.String()).Msg("Starting tunnel")

	return sc.run(tunnelID)
}

func buildCleanupCommand() *cli.Command {
	return &cli.Command{
		Name:               "cleanup",
		Action:             cliutil.ConfiguredAction(cleanupCommand),
		Usage:              "Cleanup tunnel connections",
		UsageText:          "cloudflared tunnel [tunnel command options] cleanup [subcommand options] TUNNEL",
		Description:        "Delete connections for tunnels with the given UUIDs or names.",
		Flags:              []cli.Flag{cleanupClientFlag},
		CustomHelpTemplate: commandHelpTemplate(),
	}
}

func cleanupCommand(c *cli.Context) error {
	if c.NArg() < 1 {
		return cliutil.UsageError(`"cloudflared tunnel cleanup" requires at least 1 argument, the IDs of the tunnels to cleanup connections.`)
	}

	sc, err := newSubcommandContext(c)
	if err != nil {
		return err
	}

	tunnelIDs, err := sc.findIDs(c.Args().Slice())
	if err != nil {
		return err
	}

	return sc.cleanupConnections(tunnelIDs)
}

func buildRouteCommand() *cli.Command {
	return &cli.Command{
		Name:      "route",
		Action:    cliutil.ConfiguredAction(routeCommand),
		Usage:     "Define which traffic routed from Cloudflare edge to this tunnel: requests to a DNS hostname, to a Cloudflare Load Balancer, or traffic originating from Cloudflare WARP clients",
		UsageText: "cloudflared tunnel [tunnel command options] route [subcommand options] [dns TUNNEL HOSTNAME]|[lb TUNNEL HOSTNAME LB-POOL]|[ip NETWORK TUNNEL]",
		Description: `The route command defines how Cloudflare will proxy requests to this tunnel.

To route a hostname by creating a DNS CNAME record to a tunnel:
   cloudflared tunnel route dns <tunnel ID or name> <hostname>
You can read more at: https://developers.cloudflare.com/cloudflare-one/connections/connect-apps/routing-to-tunnel/dns

To use this tunnel as a load balancer origin, creating pool and load balancer if necessary:
   cloudflared tunnel route lb <tunnel ID or name> <hostname> <load balancer pool>
You can read more at: https://developers.cloudflare.com/cloudflare-one/connections/connect-apps/routing-to-tunnel/lb

For Cloudflare WARP traffic to be routed to your private network, reachable from this tunnel as origins, use:
   cloudflared tunnel route ip <network CIDR> <tunnel ID or name>
Further information about managing Cloudflare WARP traffic to your tunnel is available at:
   cloudflared tunnel route ip --help
`,
		CustomHelpTemplate: commandHelpTemplate(),
		Subcommands: []*cli.Command{
			buildRouteIPSubcommand(),
		},
		Flags: []cli.Flag{overwriteDNSFlag},
	}
}

func dnsRouteFromArg(c *cli.Context, overwriteExisting bool) (tunnelstore.Route, error) {
	const (
		userHostnameIndex = 2
		expectedNArgs     = 3
	)
	if c.NArg() != expectedNArgs {
		return nil, cliutil.UsageError("Expected %d arguments, got %d", expectedNArgs, c.NArg())
	}
	userHostname := c.Args().Get(userHostnameIndex)
	if userHostname == "" {
		return nil, cliutil.UsageError("The third argument should be the hostname")
	} else if !validateHostname(userHostname, true) {
		return nil, errors.Errorf("%s is not a valid hostname", userHostname)
	}
	return tunnelstore.NewDNSRoute(userHostname, overwriteExisting), nil
}

func lbRouteFromArg(c *cli.Context) (tunnelstore.Route, error) {
	const (
		lbNameIndex   = 2
		lbPoolIndex   = 3
		expectedNArgs = 4
	)
	if c.NArg() != expectedNArgs {
		return nil, cliutil.UsageError("Expected %d arguments, got %d", expectedNArgs, c.NArg())
	}
	lbName := c.Args().Get(lbNameIndex)
	if lbName == "" {
		return nil, cliutil.UsageError("The third argument should be the load balancer name")
	} else if !validateHostname(lbName, true) {
		return nil, errors.Errorf("%s is not a valid load balancer name", lbName)
	}

	lbPool := c.Args().Get(lbPoolIndex)
	if lbPool == "" {
		return nil, cliutil.UsageError("The fourth argument should be the pool name")
	} else if !validateName(lbPool, false) {
		return nil, errors.Errorf("%s is not a valid pool name", lbPool)
	}

	return tunnelstore.NewLBRoute(lbName, lbPool), nil
}

var nameRegex = regexp.MustCompile("^[_a-zA-Z0-9][-_.a-zA-Z0-9]*$")
var hostNameRegex = regexp.MustCompile("^[*_a-zA-Z0-9][-_.a-zA-Z0-9]*$")

func validateName(s string, allowWildcardSubdomain bool) bool {
	if allowWildcardSubdomain {
		return hostNameRegex.MatchString(s)
	}
	return nameRegex.MatchString(s)
}

func validateHostname(s string, allowWildcardSubdomain bool) bool {
	// Slightly stricter than PunyCodeProfile
	idnaProfile := idna.New(
		idna.ValidateLabels(true),
		idna.VerifyDNSLength(true))

	puny, err := idnaProfile.ToASCII(s)
	return err == nil && validateName(puny, allowWildcardSubdomain)
}

func routeCommand(c *cli.Context) error {
	if c.NArg() < 2 {
		return cliutil.UsageError(`"cloudflared tunnel route" requires the first argument to be the route type(dns or lb), followed by the ID or name of the tunnel`)
	}
	sc, err := newSubcommandContext(c)
	if err != nil {
		return err
	}

	const tunnelIDIndex = 1

	routeType := c.Args().First()
	var route tunnelstore.Route
	var tunnelID uuid.UUID
	switch routeType {
	case "dns":
		tunnelID, err = sc.findID(c.Args().Get(tunnelIDIndex))
		if err != nil {
			return err
		}
		route, err = dnsRouteFromArg(c, c.Bool(overwriteDNSFlagName))
		if err != nil {
			return err
		}
	case "lb":
		tunnelID, err = sc.findID(c.Args().Get(tunnelIDIndex))
		if err != nil {
			return err
		}
		route, err = lbRouteFromArg(c)
		if err != nil {
			return err
		}
	default:
		return cliutil.UsageError("%s is not a recognized route type. Supported route types are dns and lb", routeType)
	}

	res, err := sc.route(tunnelID, route)
	if err != nil {
		return err
	}

	sc.log.Info().Str(LogFieldTunnelID, tunnelID.String()).Msg(res.SuccessSummary())
	return nil
}

func commandHelpTemplate() string {
	var parentFlagsHelp string
	for _, f := range configureCloudflaredFlags(false) {
		parentFlagsHelp += fmt.Sprintf(" %s\n\t", f)
	}
	for _, f := range configureLoggingFlags(false) {
		parentFlagsHelp += fmt.Sprintf(" %s\n\t", f)
	}
	const template = `NAME:
	{{.HelpName}} - {{.Usage}}

USAGE:
	{{.UsageText}}

DESCRIPTION:
	{{.Description}}

TUNNEL COMMAND OPTIONS:
	%s
SUBCOMMAND OPTIONS:
	{{range .VisibleFlags}}{{.}}
	{{end}}
`
	return fmt.Sprintf(template, parentFlagsHelp)
}
