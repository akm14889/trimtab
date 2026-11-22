# Tiltfile for local Trimtab development on Kind.
# Start with: ./scripts/local-k8s.sh up && tilt up

allow_k8s_contexts('kind-trimtab-dev')

docker_build(
    'trimtab-dev',
    '.',
    dockerfile='Dockerfile',
    only=[
        'go.mod',
        'go.sum',
        'cmd/',
        'pkg/',
        'examples/demo/',
    ],
)

k8s_yaml([
    'deploy/local/namespace.yaml',
    'deploy/local/trimtab-job.yaml',
])

k8s_resource(
    'trimtab-analyze',
    labels=['trimtab'],
)

local_resource(
    'unit-tests',
    cmd='go test -race ./...',
    auto_init=False,
    trigger_mode=TRIGGER_MODE_MANUAL,
    labels=['checks'],
)

local_resource(
    'synth-otel-preview',
    cmd='kubectl -n trimtab-local run trimtab-otel-preview --rm -i --restart=Never --image=trimtab-dev:latest --image-pull-policy=IfNotPresent -- synth --config /workspace/examples/demo/trimtab-otel.yaml --now 2026-11-21T12:00:00Z --out - --no-state-write',
    auto_init=False,
    trigger_mode=TRIGGER_MODE_MANUAL,
    labels=['trimtab'],
)

local_resource(
    'synth-promrw-preview',
    cmd='kubectl -n trimtab-local run trimtab-promrw-preview --rm -i --restart=Never --image=trimtab-dev:latest --image-pull-policy=IfNotPresent -- synth --config /workspace/examples/demo/trimtab-promrw.yaml --now 2026-11-21T12:00:00Z --out - --no-state-write',
    auto_init=False,
    trigger_mode=TRIGGER_MODE_MANUAL,
    labels=['trimtab'],
)
