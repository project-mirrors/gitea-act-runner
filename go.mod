module gitea.com/gitea/runner

go 1.27

toolchain go1.27.1

require (
	connectrpc.com/connect v1.21.0
	dario.cat/mergo v1.0.2
	gitea.dev/actionslib v1.2.1
	github.com/avast/retry-go/v5 v5.0.0
	github.com/bmatcuk/doublestar/v4 v4.10.0
	github.com/containerd/errdefs v1.0.0
	github.com/creack/pty v1.1.24
	github.com/distribution/reference v0.6.0
	github.com/docker/cli v29.8.1+incompatible
	github.com/docker/go-connections v0.8.1
	github.com/docker/go-units v0.5.0
	github.com/go-git/go-billy/v5 v5.9.1
	github.com/go-git/go-git/v5 v5.19.2
	github.com/google/go-cmp v0.7.0
	github.com/johannesboyne/gofakes3 v1.2.0
	github.com/joho/godotenv v1.5.1
	github.com/julienschmidt/httprouter v1.3.0
	github.com/kballard/go-shellquote v0.0.0-20180428030007-95032a82bc51
	github.com/mattn/go-isatty v0.0.24
	github.com/moby/go-archive v0.3.3
	github.com/moby/moby/api v1.56.0
	github.com/moby/moby/client v0.6.0
	github.com/moby/patternmatcher v0.6.1
	github.com/opencontainers/image-spec v1.1.1
	github.com/opencontainers/selinux v1.15.1
	github.com/prometheus/client_golang v1.24.1
	github.com/prometheus/client_model v0.6.3
	github.com/sirupsen/logrus v1.10.2
	github.com/spf13/cobra v1.10.2
	github.com/spf13/pflag v1.0.10
	github.com/stretchr/testify v1.12.1
	github.com/timshannon/bolthold v0.0.0-20240314194003-30aac6950928
	go.etcd.io/bbolt v1.5.0
	go.opentelemetry.io/otel v1.46.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.46.0
	go.opentelemetry.io/otel/sdk v1.46.0
	go.opentelemetry.io/otel/trace v1.46.0
	go.opentelemetry.io/proto/otlp v1.11.0
	go.yaml.in/yaml/v4 v4.0.0-rc.6
	golang.org/x/net v0.59.0
	golang.org/x/sync v0.23.0
	golang.org/x/sys v0.48.0
	golang.org/x/term v0.46.0
	golang.org/x/text v0.42.0
	google.golang.org/protobuf v1.36.12
	gotest.tools/v3 v3.5.2
	tags.cncf.io/container-device-interface v1.1.1
)

require (
	cyphar.com/go-pathrs v0.2.5 // indirect
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/ProtonMail/go-crypto v1.4.1 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cloudflare/circl v1.6.5 // indirect
	github.com/containerd/errdefs/pkg v0.3.0 // indirect
	github.com/containerd/log v0.1.0 // indirect
	github.com/cyphar/filepath-securejoin v0.7.0 // indirect
	github.com/docker/docker-credential-helpers v0.9.8 // indirect
	github.com/emirpasic/gods v1.18.1 // indirect
	github.com/felixge/httpsnoop v1.1.0 // indirect
	github.com/go-git/gcfg v1.5.1-0.20230307220236-3a3c6141e376 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/golang/groupcache v0.0.0-20241129210726-2c02b8208cf8 // indirect
	github.com/google/shlex v0.0.0-20191202100458-e7afc7fbc510 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/jbenet/go-context v0.0.0-20150711004518-d14ea06fba99 // indirect
	github.com/kevinburke/ssh_config v1.6.0 // indirect
	github.com/klauspost/compress v1.19.2 // indirect
	github.com/klauspost/cpuid/v2 v2.4.0 // indirect
	github.com/moby/docker-image-spec v1.3.1 // indirect
	github.com/moby/sys/sequential v0.7.0 // indirect
	github.com/moby/sys/user v0.4.1 // indirect
	github.com/moby/sys/userns v0.2.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/pjbgf/sha1cd v0.6.0 // indirect
	github.com/prometheus/common v0.70.1 // indirect
	github.com/prometheus/procfs v0.21.1 // indirect
	github.com/ryszard/goskiplist v0.0.0-20150312221310-2dfbae5fcf46 // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3 // indirect
	github.com/sergi/go-diff v1.4.0 // indirect
	github.com/skeema/knownhosts v1.3.2 // indirect
	github.com/stretchr/objx v0.5.3 // indirect
	github.com/xanzy/ssh-agent v0.3.3 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.70.0 // indirect
	go.opentelemetry.io/otel/metric v1.46.0 // indirect
	go.shabbyrobe.org/gocovmerge v0.0.0-20230507111327-fa4f82cfbf4d // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/tools v0.49.0 // indirect
	gopkg.in/warnings.v0 v0.1.2 // indirect
)
