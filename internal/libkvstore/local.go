package libkvstore

import (
	"context"
	"fmt"
	"time"

	"github.com/testcontainers/testcontainers-go"
)

func SetupLocalPDInstance(ctx context.Context) (string, testcontainers.Container, func(), error) {
	cleanup := func() {}
	exposedPorts := []string{"2379/tcp", "2380/tcp"}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:           "pingcap/pd:latest",
			ExposedPorts:    exposedPorts,
			AlwaysPullImage: false,
			Cmd: []string{
				`pd-server`,
				`--data-dir=/pd`,
				`--name=pd`,
				`--client-urls=http://0.0.0.0:2379`,
				`--peer-urls=http://0.0.0.0:2380`,
				`--initial-cluster=pd=http://pd:2380`,
			},
		},
		Started: false,
	})
	if err != nil {
		return "", nil, cleanup, err
	}
	cleanup = func() {
		timeout := time.Second
		container.Stop(ctx, &timeout)
	}
	err = container.Start(ctx)
	if err != nil {
		return "", nil, cleanup, err
	}

	host, err := container.Host(ctx)
	if err != nil {
		return "", nil, cleanup, err
	}

	mappedPort, err := container.MappedPort(ctx, "2379")
	if err != nil {
		return "", nil, cleanup, err
	}

	uri := fmt.Sprintf("http://%s:%s", host, mappedPort.Port())
	if !container.IsRunning() {
		return "", nil, cleanup, fmt.Errorf("pd terminated on start")
	}

	// req, err := http.NewRequest("GET", uri+"/pd/api/v1/health", nil)
	// if err != nil {
	// 	return "", nil, cleanup, err
	// }
	// res, err := http.DefaultClient.Do(req)
	// if err != nil {
	// 	return "", nil, cleanup, err
	// }
	// println(res)
	return uri, container, cleanup, nil
}

func SetupLocalTiKVInstance(ctx context.Context) (string, testcontainers.Container, func(), error) {
	con, pdContainer, cleanupPD, err := SetupLocalPDInstance(ctx)
	if err != nil {
		return con, pdContainer, cleanupPD, err
	}
	cleanup := func() {
		cleanupPD()
	}
	if pdContainer == nil {
		cleanupPD()
		panic("")
	}
	exposedPorts := []string{"20160/tcp"}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:           "pingcap/pd:latest",
			ExposedPorts:    exposedPorts,
			AlwaysPullImage: false,
			Cmd: []string{
				`tikv-server`,
				`--pd=` + con,
				`--data-dir=/tikv`,
			},
		},
		Started: false,
	})
	if err != nil {
		return "", nil, cleanup, err
	}
	cleanup = func() {
		cleanupPD()
		timeout := time.Second
		container.Stop(ctx, &timeout)
	}
	err = container.Start(ctx)
	if err != nil {
		return "", nil, cleanup, err
	}

	// host, err := container.Host(ctx)
	// if err != nil {
	// 	return "", nil, cleanup, err
	// }

	// mappedPort, err := container.MappedPort(ctx, "20160")
	// if err != nil {
	// 	return "", nil, cleanup, err
	// }

	// uri := fmt.Sprintf("http://%s:%s", host, mappedPort.Port())

	return con, pdContainer, cleanup, nil
}
