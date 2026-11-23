# shellcheck shell=bash
# Single open runtime profile contract for the iSCSI local sanity flow.
#
# The connection settings — credential, storage endpoints, pool and portal
# group — are accepted from exactly ONE open source, chosen explicitly (never
# mixed silently):
#
#   * FILE MODE  — tests/sanity/local/config.env EXISTS. The file is sourced
#     EXACTLY ONCE, in a cleared scope (the four keys unset), and must provide
#     all four keys on its own: a `${VAR:-}` template line cannot inherit a
#     stale runtime, and a partial file masked by a full inherited
#     environment is rejected. The four validated values travel to the caller
#     through a dedicated 0600 temp file; the config's stdout/stderr are
#     discarded and never echoed — diagnostics carry only key NAMES, never
#     values. Contract: the profile file defines exactly these four keys;
#     coordination settings are loaded separately by the dispatcher and are
#     NOT part of the validated profile.
#   * ENV MODE   — config.env DOES NOT EXIST. The four keys must all be
#     present and non-empty in the exported environment.
#
# There are no silent defaults and no fallback channel that scrapes values
# from documentation (the former Tests/infrastructure markdown lab scrape is
# gone for good). A profile that mixes settings from different runtimes (for
# example a credential for the new array with endpoints inherited from the
# old one) fails preflight instead of connecting somewhere unintended.
#
# Limitation (documented, NOT verified here): the checks prove the four
# fields are present and non-empty from one source. They do NOT prove the
# credential, endpoints, pool and portal group all belong to the SAME
# storage array — that correspondence is operator knowledge and can only be
# confirmed against the live runtime, which this offline preflight never
# touches.
#
# Ported from the NFS runtime-profile contract (V3 semantics): load,
# validation and transfer happen in one cleared execution with separate,
# bounded channels; a config that fails or leaks to stderr is rejected
# without echoing its raw output.

# sanity_profile_preflight <config.env>
# Picks the profile source explicitly (file mode when the file exists, env
# mode otherwise) and requires all four connection settings to be non-empty
# from that single source. Returns 2 with a FIXED diagnostic naming only the
# missing key names (never a value) when the chosen profile is absent,
# partial, broken or syntactically invalid; exports the verified profile on
# success. Makes no live call.
sanity_profile_preflight() {
    local config_file="$1"
    if [ -f "$config_file" ]; then
        local rc=0 dat err miss
        # Transfer scratch lives under $TMPDIR (home cache on this machine),
        # mode 0600, removed before return.
        dat="$(mktemp "${TMPDIR:-$HOME/.cache/ngxstorage}/quartet-profile.XXXXXX")" || return 2
        chmod 600 "$dat" 2>/dev/null
        err="${dat}.err"; miss="${dat}.missing"
        : > "$err"; : > "$miss"
        # ONE execution, cleared scope: source once (stdout discarded), then
        # validate; the four values go to $dat, missing key NAMES to $miss
        # (fd 3), and the raw config stderr to $err — none of err/miss is
        # ever printed wholesale.
        env -u STORAGE_API_KEY -u STORAGE_IPS -u STORAGE_POOL_NAME -u STORAGE_PORTAL_GROUP bash -c '
            set -a
            source "$1" >/dev/null 2>"$2" || exit 17
            set +a
            incomplete=0
            for k in STORAGE_API_KEY STORAGE_IPS STORAGE_POOL_NAME STORAGE_PORTAL_GROUP; do
                if [ -z "${!k:-}" ]; then printf "%s\n" "$k" >&3; incomplete=2; fi
            done
            [ $incomplete -eq 0 ] || exit $incomplete
            printf "%s\n%s\n%s\n%s\n" "$STORAGE_IPS" "$STORAGE_POOL_NAME" "$STORAGE_PORTAL_GROUP" "$STORAGE_API_KEY" > "$4"
        ' _ "$config_file" "$err" "$miss" "$dat" 3>>"$miss" || rc=$?
        if [ $rc -ne 0 ] || [ ! -s "$dat" ]; then
            {
                echo "sanity preflight failed: config.env is incomplete or unreadable (file mode)"
                if [ -s "$miss" ]; then
                    echo "(keys the file does not provide: $(tr '\n' ' ' < "$miss"))"
                fi
                echo "In file mode the profile is authoritative — do not mix it"
                echo "with inherited environment values. Fill every field in"
                echo "$config_file, or delete it to use ENV MODE. See config.env.example."
            } >&2
            rm -f -- "$dat" "$err" "$miss"
            return 2
        fi
        local -a _vals=()
        mapfile -t _vals < "$dat"
        rm -f -- "$dat" "$err" "$miss"
        if [ "${#_vals[@]}" -ne 4 ]; then
            echo "sanity preflight failed: profile transfer produced ${#_vals[@]} values, expected 4" >&2
            return 2
        fi
        STORAGE_IPS="${_vals[0]}"
        STORAGE_POOL_NAME="${_vals[1]}"
        STORAGE_PORTAL_GROUP="${_vals[2]}"
        STORAGE_API_KEY="${_vals[3]}"
        export STORAGE_API_KEY STORAGE_IPS STORAGE_POOL_NAME STORAGE_PORTAL_GROUP
        return 0
    fi
    # ENV MODE: no config file — require all four keys from the environment.
    local key missing=""
    for key in STORAGE_API_KEY STORAGE_IPS STORAGE_POOL_NAME STORAGE_PORTAL_GROUP; do
        if [ -z "${!key:-}" ]; then
            missing="$missing $key"
        fi
    done
    if [ -n "$missing" ]; then
        {
            echo "sanity preflight failed: incomplete runtime profile (ENV mode, missing:$missing)"
            echo "Provide one complete profile: fill tests/sanity/local/config.env"
            echo "(file mode) OR export all four keys (env mode). They must all"
            echo "describe the SAME storage runtime. See config.env.example."
        } >&2
        return 2
    fi
    export STORAGE_API_KEY STORAGE_IPS STORAGE_POOL_NAME STORAGE_PORTAL_GROUP
}