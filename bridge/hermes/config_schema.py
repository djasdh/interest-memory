"""interest-memory bridge config surface (declarative schema).

Rendered by the generic desktop config panel; keys map to env vars.
"""

from plugins.memory.config_schema import (
    KIND_TEXT,
    ProviderConfigSchema,
    ProviderField,
)

CONFIG_SCHEMA = ProviderConfigSchema(
    name="interest",
    label="interest-memory",
    docs_url="https://github.com/anomalyco/interest-memory",
    fields=(
        ProviderField(
            key="base_url",
            label="Service base URL",
            kind=KIND_TEXT,
            default="http://127.0.0.1:8899",
            description="interest-memory service base URL.",
            env_key="INTEREST_BASE_URL",
            inline=True,
        ),
        ProviderField(
            key="agent",
            label="Agent namespace",
            kind=KIND_TEXT,
            default="",
            description="Agent namespace override (defaults to the Hermes profile).",
            env_key="INTEREST_AGENT",
        ),
        ProviderField(
            key="timeout",
            label="Request timeout (seconds)",
            kind=KIND_TEXT,
            default="8",
            description="Per-request timeout for recall/ingest calls.",
            env_key="INTEREST_TIMEOUT",
        ),
    ),
)
