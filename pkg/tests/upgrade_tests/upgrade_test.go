// This file and its contents are licensed under the Apache License 2.0.
// Please see the included NOTICE for copyright information and
// LICENSE for a copy of the license.
package upgrade_tests

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"testing"

	"github.com/blang/semver/v4"
	"github.com/docker/go-connections/nat"
	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/jackc/pgx/v4"
	"github.com/jackc/pgx/v4/pgxpool"
	_ "github.com/jackc/pgx/v4/stdlib"
	"github.com/testcontainers/testcontainers-go"
	"github.com/timescale/promscale/pkg/internal/testhelpers"
	"github.com/timescale/promscale/pkg/log"
	"github.com/timescale/promscale/pkg/pgmodel"
	"github.com/timescale/promscale/pkg/pgmodel/common/extension"
	"github.com/timescale/promscale/pkg/pgmodel/ingestor"
	"github.com/timescale/promscale/pkg/pgmodel/model"
	"github.com/timescale/promscale/pkg/pgxconn"
	"github.com/timescale/promscale/pkg/prompb"
	"github.com/timescale/promscale/pkg/runner"
	"github.com/timescale/promscale/pkg/version"
)

var (
	testDatabase       = flag.String("database", "tmp_db_timescale_upgrade_test", "database to run integration tests on")
	useExtension       = flag.Bool("use-extension", true, "use the promscale extension")
	printLogs          = flag.Bool("print-logs", false, "print TimescaleDB logs")
	baseExtensionState testhelpers.ExtensionState
)

func TestMain(m *testing.M) {
	var code int
	flag.Parse()
	baseExtensionState.UseTimescaleDB()
	baseExtensionState.UseTimescale2()
	baseExtensionState.UsePG12()
	if *useExtension {
		baseExtensionState.UsePromscale()
	}
	_ = log.Init(log.Config{
		Level: "debug",
	})
	code = m.Run()
	os.Exit(code)
}

/* Prev image is the db image with the old promscale extension. We do NOT test timescaleDB extension upgrades here. */
func getDBImages(extensionState testhelpers.ExtensionState) (prev string, clean string) {
	if !extensionState.UsesPG12() {
		//TODO add tests after release with PG13 support
		panic("Can't test PG13 yet because haven't had a PG13 release")
	}
	switch {
	case extensionState.UsesMultinode():
		return "timescaledev/promscale-extension:0.1.1-ts2-pg12", "timescaledev/promscale-extension:latest-ts2-pg12"
	case !extensionState.UsesTimescaleDB():
		return "timescale/timescaledb:latest-pg12", "timescale/timescaledb:latest-pg12"
	case extensionState.UsesTimescale2():
		return "timescaledev/promscale-extension:0.1.1-ts2-pg12", "timescaledev/promscale-extension:latest-ts2-pg12"
	default:
		return "timescaledev/promscale-extension:0.1.1-ts1-pg12", "timescaledev/promscale-extension:latest-ts1-pg12"
	}
}

func TestUpgradeFromPrev(t *testing.T) {
	upgradedDbInfo := getUpgradedDbInfo(t, false, baseExtensionState)
	pristineDbInfo := getPristineDbInfo(t, false, baseExtensionState)

	if !reflect.DeepEqual(pristineDbInfo, upgradedDbInfo) {
		PrintDbSnapshotDifferences(t, pristineDbInfo, upgradedDbInfo)
	}
}

func TestUpgradeFromPrevMultinode(t *testing.T) {
	extState := baseExtensionState
	extState.UseMultinode()
	upgradedDbInfo := getUpgradedDbInfo(t, false, extState)
	pristineDbInfo := getPristineDbInfo(t, false, extState)

	if !reflect.DeepEqual(pristineDbInfo, upgradedDbInfo) {
		PrintDbSnapshotDifferences(t, pristineDbInfo, upgradedDbInfo)
	}
}

// TestUpgradeFromPrevNoData tests migrations with no ingested data.
// See issue: https://github.com/timescale/promscale/issues/330
func TestUpgradeFromPrevNoData(t *testing.T) {
	upgradedDbInfo := getUpgradedDbInfo(t, true, baseExtensionState)
	pristineDbInfo := getPristineDbInfo(t, true, baseExtensionState)

	if !reflect.DeepEqual(pristineDbInfo, upgradedDbInfo) {
		PrintDbSnapshotDifferences(t, pristineDbInfo, upgradedDbInfo)
	}
}

func getUpgradedDbInfo(t *testing.T, noData bool, extensionState testhelpers.ExtensionState) (upgradedDbInfo dbSnapshot) {
	// we test that upgrading from the previous version gives the correct output
	// by induction, this property should hold true for any chain of versions
	prevVersion := semver.MustParse(version.EarliestUpgradeTestVersion)
	if extensionState.UsesMultinode() || extensionState.UsesTimescale2() {
		prevVersion = semver.MustParse(version.EarliestUpgradeTestVersionMultinode)
	}
	// TODO we could probably improve performance of this test by 2x if we
	//      gathered the db info in parallel. Unfortunately our db runner doesn't
	//      support this yet
	withDBStartingAtOldVersionAndUpgrading(t, *testDatabase, prevVersion, extensionState,
		/* preUpgrade */
		func(dbContainer testcontainers.Container, dbTmpDir string, connectorHost string, connectorPort nat.Port) {
			if noData {
				return
			}
			client := http.Client{}
			defer client.CloseIdleConnections()

			writeUrl := fmt.Sprintf("http://%s/write", net.JoinHostPort(connectorHost, connectorPort.Port()))

			doWrite(t, &client, writeUrl, preUpgradeData1, preUpgradeData2)
		},
		/* postUpgrade */
		func(dbContainer testcontainers.Container, dbTmpDir string) {
			connectURL := testhelpers.PgConnectURL(*testDatabase, testhelpers.NoSuperuser)

			db, err := pgxpool.Connect(context.Background(), connectURL)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()

			if !noData {
				ing, err := ingestor.NewPgxIngestorForTests(pgxconn.NewPgxConn(db))
				if err != nil {
					t.Fatalf("error connecting to DB: %v", err)
				}

				doIngest(t, ing, postUpgradeData1, postUpgradeData2)

				ing.Close()

			}
			upgradedDbInfo = SnapshotDB(t, dbContainer, *testDatabase, dbTmpDir, db, extensionState)
		})
	return
}

func getPristineDbInfo(t *testing.T, noData bool, extensionState testhelpers.ExtensionState) (pristineDbInfo dbSnapshot) {
	withNewDBAtCurrentVersion(t, *testDatabase, extensionState,
		/* preRestart */
		func(container testcontainers.Container, _ string, db *pgxpool.Pool, tmpDir string) {
			if noData {
				return
			}
			ing, err := ingestor.NewPgxIngestorForTests(pgxconn.NewPgxConn(db))
			if err != nil {
				t.Fatalf("error connecting to DB: %v", err)
			}
			defer ing.Close()

			doIngest(t, ing, preUpgradeData1, preUpgradeData2)
		},
		/* postRestart */
		func(container testcontainers.Container, _ string, db *pgxpool.Pool, tmpDir string) {
			if !noData {
				ing, err := ingestor.NewPgxIngestorForTests(pgxconn.NewPgxConn(db))
				if err != nil {
					t.Fatalf("error connecting to DB: %v", err)
				}
				defer ing.Close()

				doIngest(t, ing, postUpgradeData1, postUpgradeData2)
			}
			pristineDbInfo = SnapshotDB(t, container, *testDatabase, tmpDir, db, extensionState)
		})
	return
}

// pick a start time in the future so data won't get compressed
const startTime = 6600000000000 // approx 210 years after the epoch
var (
	preUpgradeData1 = []prompb.TimeSeries{
		{
			Labels: []prompb.Label{
				{Name: model.MetricNameLabelName, Value: "test"},
				{Name: "test", Value: "test"},
			},
			Samples: []prompb.Sample{
				{Timestamp: startTime + 1, Value: 0.1},
				{Timestamp: startTime + 2, Value: 0.2},
			},
		},
	}
	preUpgradeData2 = []prompb.TimeSeries{
		{
			Labels: []prompb.Label{
				{Name: model.MetricNameLabelName, Value: "test2"},
				{Name: "foo", Value: "bar"},
			},
			Samples: []prompb.Sample{
				{Timestamp: startTime + 4, Value: 2.2},
			},
		},
	}

	postUpgradeData1 = []prompb.TimeSeries{
		{
			Labels: []prompb.Label{
				{Name: model.MetricNameLabelName, Value: "test"},
				{Name: "testB", Value: "testB"},
			},
			Samples: []prompb.Sample{
				{Timestamp: startTime + 4, Value: 0.4},
				{Timestamp: startTime + 5, Value: 0.5},
			},
		},
	}
	postUpgradeData2 = []prompb.TimeSeries{
		{
			Labels: []prompb.Label{
				{Name: model.MetricNameLabelName, Value: "test3"},
				{Name: "baz", Value: "quf"},
			},
			Samples: []prompb.Sample{
				{Timestamp: startTime + 66, Value: 6.0},
			},
		},
	}
)

func addNode2(t testing.TB, DBName string) {
	db, err := pgx.Connect(context.Background(), testhelpers.PgConnectURL(DBName, testhelpers.Superuser))
	if err != nil {
		t.Fatal(err)
	}
	err = testhelpers.AddDataNode2(db, DBName)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	//do this as prom user
	dbProm, err := pgx.Connect(context.Background(), testhelpers.PgConnectURL(DBName, testhelpers.NoSuperuser))
	if err != nil {
		t.Fatal(err)
	}
	_, err = dbProm.Exec(context.Background(), "CALL add_prom_node('dn1');")
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Start a db with the prev extra extension and a prev connector as well.
// This ensures that we test upgrades of both the extension and the connector schema.
// Then run preUpgrade and shut everything down.
// Start a new db with the latest extra extension and migrate to the latest version of the connector schema.
// Then run postUpgrade.
func withDBStartingAtOldVersionAndUpgrading(
	t testing.TB,
	DBName string,
	prevVersion semver.Version,
	extensionState testhelpers.ExtensionState,
	preUpgrade func(dbContainer testcontainers.Container, dbTmpDir string, connectorHost string, connectorPort nat.Port),
	postUpgrade func(dbContainer testcontainers.Container, dbTmpDir string)) {
	var err error
	ctx := context.Background()

	tmpDir, err := testhelpers.TempDir("update_test_out")
	if err != nil {
		log.Fatal(err)
	}

	dataDir, err := testhelpers.TempDir("update_test_data")
	if err != nil {
		log.Fatal(err)
	}

	prevDBImage, cleanImage := getDBImages(extensionState)
	// Start a db with the prev extension and a prev connector as well
	// Then run preUpgrade and shut everything down.
	func() {
		dbContainer, closer, err := testhelpers.StartDatabaseImage(ctx, prevDBImage, tmpDir, dataDir, *printLogs, extensionState)
		if err != nil {
			t.Fatal("Error setting up container", err)
		}

		defer func() { _ = closer.Close() }()

		db, err := testhelpers.DbSetup(*testDatabase, testhelpers.NoSuperuser, true, extensionState)
		if err != nil {
			t.Fatal(err)
			return
		}
		db.Close()

		connectorImage := "timescale/promscale:" + prevVersion.String()
		connector, err := testhelpers.StartConnectorWithImage(context.Background(), dbContainer, connectorImage, *printLogs, []string{}, *testDatabase)
		if err != nil {
			log.Fatal(err.Error())
		}
		defer testhelpers.StopContainer(ctx, connector, *printLogs)

		connectorHost, err := connector.Host(ctx)
		if err != nil {
			t.Fatal(err)
			return
		}

		connectorPort, err := connector.MappedPort(ctx, testhelpers.ConnectorPort)
		if err != nil {
			t.Fatal(err)
			return
		}
		t.Logf("Running preUpgrade with old version of connector and db: connector=%v db=%v", connectorImage, prevDBImage)
		preUpgrade(dbContainer, tmpDir, connectorHost, connectorPort)
	}()

	//Start a new connector and migrate.
	//Then run postUpgrade
	dbContainer, closer, err := testhelpers.StartDatabaseImage(ctx, cleanImage, tmpDir, dataDir, *printLogs, extensionState)
	if err != nil {
		t.Fatal("Error setting up container", err)
	}

	defer func() { _ = closer.Close() }()

	t.Logf("upgrading versions %v => %v", prevVersion, version.Version)
	connectURL := testhelpers.PgConnectURL(*testDatabase, testhelpers.NoSuperuser)
	migrateToVersion(t, connectURL, version.Version, "azxtestcommit")

	if extensionState.UsesMultinode() {
		//add a node after upgrade; this tests strictly more functionality since we already have one node set up before
		addNode2(t, *testDatabase)
	}
	t.Log("Running postUpgrade")
	postUpgrade(dbContainer, tmpDir)

}

// Run a DB and connector at the current version. Run preRestart then restart the db
// then run postRestart. A restart is necessary because we need a restart in the
// upgrade path to change the extension that is available. But, a restart causes
// Sequences to skip values. So, in order to have equivalent data, we need to make
// sure that both the upgrade and this pristine path both have restarts.
func withNewDBAtCurrentVersion(t testing.TB, DBName string, extensionState testhelpers.ExtensionState,
	preRestart func(container testcontainers.Container, connectURL string, db *pgxpool.Pool, tmpDir string),
	postRestart func(container testcontainers.Container, connectURL string, db *pgxpool.Pool, tmpDir string)) {
	var err error
	ctx := context.Background()

	tmpDir, err := testhelpers.TempDir("update_test_out")
	if err != nil {
		log.Fatal(err)
	}
	dataDir, err := testhelpers.TempDir("update_test_data")
	if err != nil {
		log.Fatal(err)
	}

	_, cleanImage := getDBImages(extensionState)

	func() {
		container, closer, err := testhelpers.StartDatabaseImage(ctx, cleanImage, tmpDir, dataDir, *printLogs, extensionState)
		if err != nil {
			fmt.Println("Error setting up container", err)
			os.Exit(1)
		}

		defer func() { _ = closer.Close() }()
		testhelpers.WithDB(t, DBName, testhelpers.NoSuperuser, true, extensionState, func(_ *pgxpool.Pool, t testing.TB, connectURL string) {
			migrateToVersion(t, connectURL, version.Version, "azxtestcommit")

			// need to get a new pool after the Migrate to catch any GUC changes made during Migrate
			db, err := pgxpool.Connect(context.Background(), connectURL)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			preRestart(container, connectURL, db, tmpDir)
		})
	}()
	container, closer, err := testhelpers.StartDatabaseImage(ctx, cleanImage, tmpDir, dataDir, *printLogs, extensionState)
	if err != nil {
		fmt.Println("Error setting up container", err)
		os.Exit(1)
	}

	if extensionState.UsesMultinode() {
		addNode2(t, *testDatabase)
	}
	defer func() { _ = closer.Close() }()
	connectURL := testhelpers.PgConnectURL(*testDatabase, testhelpers.NoSuperuser)
	db, err := pgxpool.Connect(context.Background(), connectURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	postRestart(container, connectURL, db, tmpDir)
}

func migrateToVersion(t testing.TB, connectURL string, version string, commitHash string) {
	err := extension.InstallUpgradeTimescaleDBExtensions(connectURL, extension.ExtensionMigrateOptions{Install: true, Upgrade: true})
	if err != nil {
		t.Fatal(err)
	}

	migratePool, err := pgx.Connect(context.Background(), connectURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = migratePool.Close(context.Background()) }()
	err = runner.SetupDBState(migratePool, pgmodel.VersionInfo{Version: version, CommitHash: commitHash}, nil, extension.ExtensionMigrateOptions{Install: true, Upgrade: true})
	if err != nil {
		t.Fatal(err)
	}
}

func tsWriteReq(ts []prompb.TimeSeries) prompb.WriteRequest {
	return prompb.WriteRequest{
		Timeseries: ts,
	}
}

func writeReqToHttp(r prompb.WriteRequest) *bytes.Reader {
	data, _ := proto.Marshal(&r)
	body := snappy.Encode(nil, data)
	return bytes.NewReader(body)
}

func doWrite(t *testing.T, client *http.Client, url string, data ...[]prompb.TimeSeries) {
	for _, data := range data {
		body := writeReqToHttp(tsWriteReq(copyMetrics(data)))
		req, err := http.NewRequest("POST", url, body)
		if err != nil {
			t.Errorf("Error creating request: %v", err)
		}
		req.Header.Add("Content-Encoding", "snappy")
		req.Header.Set("Content-Type", "application/x-protobuf")
		req.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 {
			t.Fatal("non-ok status:", resp.Status)
		}

		_, _ = io.Copy(ioutil.Discard, resp.Body)
		resp.Body.Close()
	}
}

func doIngest(t *testing.T, ingestor *ingestor.DBIngestor, data ...[]prompb.TimeSeries) {
	for _, data := range data {
		_, err := ingestor.Ingest(copyMetrics(data), &prompb.WriteRequest{})
		if err != nil {
			t.Fatalf("ingest error: %v", err)
		}
		_ = ingestor.CompleteMetricCreation()
	}
}

// deep copy the metrics since we mutate them, and don't want to invalidate the tests
func copyMetrics(metrics []prompb.TimeSeries) []prompb.TimeSeries {
	out := make([]prompb.TimeSeries, len(metrics))
	copy(out, metrics)
	for i := range out {
		out[i].Labels = make([]prompb.Label, len(metrics[i].Labels))
		out[i].Samples = make([]prompb.Sample, len(metrics[i].Samples))
		copy(out[i].Labels, metrics[i].Labels)
		copy(out[i].Samples, metrics[i].Samples)
	}
	return out
}

func TestExtensionUpgrade(t *testing.T) {
	var err error
	var version string
	ctx := context.Background()
	buildPromscaleImageFromRepo(t)
	db, dbContainer, closer := startDB(t, ctx)
	defer closer.Close()

	defer testhelpers.StopContainer(ctx, dbContainer, false)

	// as the default installed version ext is rc4 in the test image downgrade it to rc2
	// to test upgrade flow.
	extVersion := "2.0.0-rc2"
	dropAndCreateExt(t, ctx, extVersion)

	db, err = pgx.Connect(ctx, testhelpers.PgConnectURL("postgres", testhelpers.Superuser))
	if err != nil {
		t.Fatal(err)
	}

	err = db.QueryRow(ctx, `SELECT extversion FROM pg_extension where extname='timescaledb'`).Scan(&version)
	if err != nil {
		t.Fatal(err)
	}

	if version != extVersion {
		t.Fatal("failed to verify upgrade extension with -upgrade-prerelease-extension false")
	}

	// start promscale & test upgrade-prerelease-extensions as false
	// Now the ext is rc2 it should be rc2 after promscale startup too
	func() {
		connectorImage := "timescale/promscale:latest"
		databaseName := "postgres"
		connector, err := testhelpers.StartConnectorWithImage(ctx, dbContainer, connectorImage, *printLogs, []string{}, databaseName)
		if err != nil {
			t.Fatal(err)
		}
		defer testhelpers.StopContainer(ctx, connector, *printLogs)
		err = db.QueryRow(ctx, `SELECT extversion FROM pg_extension where extname='timescaledb'`).Scan(&version)
		if err != nil {
			t.Fatal(err)
		}

		if version != extVersion {
			t.Fatal("failed to verify upgrade extension with -upgrade-prerelease-extension false")
		}
		t.Logf("successfully tested extension upgrade flow with --upgrade-prereleases-extensions false")
	}()

	db.Close(ctx)

	// start a new connector and test --upgrade-prerelease-extensions as true
	// the default installed ext version is rc2 now it should upgrade it to rc4
	func() {
		connectorImage := "timescale/promscale:latest"
		databaseName := "postgres"
		flags := []string{"-upgrade-prerelease-extensions", "true"}
		connector, err := testhelpers.StartConnectorWithImage(ctx, dbContainer, connectorImage, *printLogs, flags, databaseName)
		if err != nil {
			t.Fatal(err)
		}
		defer testhelpers.StopContainer(ctx, connector, *printLogs)

		var versionStr string
		db, err = pgx.Connect(ctx, testhelpers.PgConnectURL("postgres", testhelpers.Superuser))
		if err != nil {
			t.Fatal(err)
		}
		err = db.QueryRow(ctx, `SELECT extversion FROM pg_extension where extname='timescaledb'`).Scan(&versionStr)
		if err != nil {
			t.Fatal(err)
		}

		db.Close(ctx)

		if versionStr != "2.0.0-rc4" {
			t.Fatal("failed to verify upgrade extension with -upgrade-prerelease-extension true")
		}
		t.Logf("successfully tested extension upgrade flow with --upgrade-prereleases-extensions true")
	}()
}

func TestMigrationFailure(t *testing.T) {
	ctx := context.Background()
	var err error
	var version string
	buildPromscaleImageFromRepo(t)
	db, dbContainer, closer := startDB(t, ctx)
	defer testhelpers.StopContainer(ctx, dbContainer, false)

	defer closer.Close()

	// start promscale & test upgrade-extensions as false
	func() {
		connectorImage := "timescale/promscale:latest"
		databaseName := "postgres"
		connector, err := testhelpers.StartConnectorWithImage(ctx, dbContainer, connectorImage, *printLogs, []string{}, databaseName)
		if err != nil {
			t.Fatal(err)
		}
		defer testhelpers.StopContainer(ctx, connector, *printLogs)

		err = db.QueryRow(ctx, `SELECT extversion FROM pg_extension where extname='timescaledb'`).Scan(&version)
		if err != nil {
			t.Fatal(err)
		}

		db.Close(ctx)
		if version != "2.0.0-rc4" {
			t.Fatal("failed to verify upgrade extension with -upgrade-prerelease-extension false")
		}
		t.Logf("successfully tested extension upgrade flow with --upgrade-prereleases-extensions false.")
	}()

	// As the timescaleDB installed version is rc4, lets install the 1.7.3 ext version
	extVersion := "1.7.3"
	dropAndCreateExt(t, ctx, extVersion)

	db, err = pgx.Connect(ctx, testhelpers.PgConnectURL("postgres", testhelpers.Superuser))
	if err != nil {
		t.Fatal(err)
	}

	err = db.QueryRow(ctx, `SELECT extversion FROM pg_extension where extname='timescaledb'`).Scan(&version)
	if err != nil {
		t.Fatal(err)
	}

	db.Close(ctx)
	if version != extVersion {
		t.Fatal("failed to verify upgrade extension with -upgrade-prerelease-extension false")
	}

	// start a new connector and test --upgrade-extensions as true which is by default set in flags
	// the migration should fail (upgrade path in tsdb isn't available) but promscale should be running.
	func() {
		connectorImage := "timescale/promscale:latest"
		databaseName := "postgres"
		connector, err := testhelpers.StartConnectorWithImage(ctx, dbContainer, connectorImage, *printLogs, []string{}, databaseName)
		if err != nil {
			t.Fatal(err)
		}
		defer testhelpers.StopContainer(ctx, connector, *printLogs)

		var version string
		db, err = pgx.Connect(ctx, testhelpers.PgConnectURL("postgres", testhelpers.Superuser))
		if err != nil {
			t.Fatal(err)
		}
		err = db.QueryRow(ctx, `SELECT extversion FROM pg_extension where extname='timescaledb'`).Scan(&version)
		if err != nil {
			t.Fatal(err)
		}
		db.Close(ctx)

		if version != "1.7.3" {
			t.Fatal("failed to verify timescaleDB extension version")
		}

		// Now from the check we are know that migration failed from 1.7.3 to 1.7.4
		// as the upgrade script doesn't exist within timescaleDB image.
		// Now check promscale is still running on migration failure.
		exitCode, err := connector.Exec(context.Background(), []string{"echo", "hello"})
		if exitCode != 0 || err != nil {
			t.Fatal("promscale failed to run extension migration failure", err)
		}
		t.Logf("successfully tested extension upgrade flow with --upgrade-prereleases-extensions true where migration fails and promscale keeps running.")
	}()
}

func buildPromscaleImageFromRepo(t *testing.T) {
	t.Logf("building promscle image from the codebase")
	cmd := exec.Command("docker", "build", "-t", "timescale/promscale:latest", "./../../../", "--file", "./../../../build/Dockerfile")
	err := cmd.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("successfully built promscale:latest image from codebase.")
}

func startDB(t *testing.T, ctx context.Context) (*pgx.Conn, testcontainers.Container, io.Closer) {
	tmpDir, err := testhelpers.TempDir("update_test_out")
	if err != nil {
		t.Fatal(err)
	}

	dataDir, err := testhelpers.TempDir("update_test_data")
	if err != nil {
		t.Fatal(err)
	}

	dbContainer, closer, err := testhelpers.StartDatabaseImage(ctx, "timescaledev/promscale-extension:testing-extension-upgrade", tmpDir, dataDir, *printLogs, testhelpers.Timescale1AndPromscale)
	if err != nil {
		t.Fatal("Error setting up container", err)
	}

	// need to get a new pool after the Migrate to catch any GUC changes made during Migrate
	db, err := pgx.Connect(ctx, testhelpers.PgConnectURL("postgres", testhelpers.Superuser))
	if err != nil {
		t.Fatal(err)
	}

	return db, dbContainer, closer
}

func dropAndCreateExt(t *testing.T, ctx context.Context, extVersion string) {
	// Drop existing installed extension & install a lower extension version to test upgrade
	db, err := pgx.Connect(ctx, testhelpers.PgConnectURL("postgres", testhelpers.Superuser))
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.Exec(ctx, `DROP EXTENSION timescaledb`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close(ctx)

	db, err = pgx.Connect(ctx, testhelpers.PgConnectURL("postgres", testhelpers.Superuser))
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.Exec(ctx, fmt.Sprintf(`CREATE EXTENSION timescaledb version '%s'`, extVersion))
	if err != nil {
		t.Fatal(err)
	}
	db.Close(ctx)
}
