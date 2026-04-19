#!/usr/bin/env sh
set -euo pipefail

# Nenya AI Gateway — Installer
# Usage: curl -fsSL https://raw.githubusercontent.com/gumieri/nenya/main/install.sh | sh
# Docs:   https://github.com/gumieri/nenya

GITHUB_REPO="gumieri/nenya"
BIN_NAME="nenya"
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/nenya"
SERVICE_DIR="/etc/systemd/system"
SERVICE_FILE="nenya.service"
MIN_GO_VERSION="1.23"

info()  { printf "\033[1;34m[info]\033[0m %s\n" "$1"; }
warn()  { printf "\033[1;33m[warn]\033[0m %s\n" "$1"; }
error() { printf "\033[1;31m[error]\033[0m %s\n" "$1" >&2; exit 1; }
success() { printf "\033[1;32m[ok]\033[0m %s\n" "$1"; }

confirm() {
	printf "%s [y/N] " "$1"
	read -r answer
	case "$answer" in
		[yY]*) return 0 ;;
	*) return 1 ;;
	esac
}

cleanup() {
	if [ -n "${TMPDIR:-}" ] && [ -d "$TMPDIR" ]; then
		rm -rf "$TMPDIR"
	fi
}
trap cleanup EXIT

check_deps() {
	for cmd in curl sha256sum tar; do
		if ! command -v "$cmd" >/dev/null 2>&1; then
			error "Required command '$cmd' not found. Install it first."
		fi
	done
}

detect_os() {
	case "$(uname -s)" in
		Linux*)   echo "linux" ;;
		Darwin*) echo "darwin" ;;
		*)       error "Unsupported operating system: $(uname -s). Nenya supports Linux and macOS." ;;
	esac
}

detect_arch() {
	case "$(uname -m)" in
	*x86_64*|amd64) echo "amd64" ;;
	*aarch64*|arm64)  echo "arm64" ;;
	*)               error "Unsupported architecture: $(uname -m). Nenya supports amd64 and arm64." ;;
	esac
}

check_go_version() {
	if ! command -v go >/dev/null 2>&1; then
		return
	fi
	go_version="$(go version 2>/dev/null | sed -n 's/Go //p')"
	major="${go_version%%.*}"
	minor="${go_version#*.}"
	if [ -n "$major" ] && [ "$major" -lt "$MIN_GO_VERSION" ]; then
		warn "Go $major.$minor found but Nenya requires Go >= $MIN_GO_VERSION. Building from source may fail."
	fi
}

resolve_version() {
	if [ -n "${VERSION:-}" ]; then
		return
	fi
	latest_url="https://github.com/${GITHUB_REPO}/releases/latest"
	latest_tag="$(curl -sfI "$latest_url" 2>/dev/null)" || true
	if [ -z "$latest_tag" ]; then
		error "Could not determine latest version. Use -v <version> to specify."
	fi
	VERSION="${latest_tag#v}"
	info "Latest version: ${VERSION}"
}

verify_download() {
	expected="$1"
	actual="$(sha256sum "$2" 2>/dev/null | cut -d' ' -f1)"
	if [ "$actual" != "$expected" ]; then
		rm -f "$2"
		error "Checksum verification failed for $(basename "$2").
Expected: $expected
Got:      $actual

The file may have been corrupted or tampered with.
Download it manually from:
  https://github.com/${GITHUB_REPO}/releases/tag/${VERSION}"
	fi
}

install_binary() {
	local src="$1"
	local dest="$INSTALL_DIR/$BIN_NAME"

	if [ -f "$dest" ]; then
		current_version="$("$dest" --version 2>/dev/null)" || true
		if [ "$current_version" = "$VERSION" ]; then
			warn "nenya ${VERSION} is already installed. Use -v <version> to force reinstall."
			return 0
		fi
		if [ "$current_version" != "" ]; then
			printf "nenya %s is installed. Replace with %s? [y/N] " "$current_version" "$VERSION"
			if ! confirm "Replace $current_version with $VERSION"; then
				info "Skipped binary installation."
				return 0
			fi
		else
			printf "A file named '%s' exists at %s. Replace it? [y/N] " "$BIN_NAME" "$dest"
			if ! confirm "Replace existing file at $dest"; then
				info "Skipped binary installation."
				return 0
			fi
		fi
	fi

	if [ ! -w "$INSTALL_DIR" ] 2>/dev/null; then
		info "Installing to $INSTALL_DIR (requires sudo)..."
		sudo_install_cmd="sudo install -Dm755"
	else
		sudo_install_cmd="install -m755"
	fi

	$sudo_install_cmd "$src" "$dest"
	success "Installed ${BIN_NAME} ${VERSION} to ${dest}."
}

install_config() {
	if [ ! -d "$CONFIG_DIR" ]; then
		info "Creating config directory ${CONFIG_DIR}..."
		sudo mkdir -p "$CONFIG_DIR"
		sudo chmod 0755 "$CONFIG_DIR"
	fi

	if ls "$CONFIG_DIR"/*.json >/dev/null 2>&1; then
		info "Existing config found in ${CONFIG_DIR}/ — not modifying."
		return 0
	fi

	local src="$1"
	local dest="$CONFIG_DIR/config.json.example"
	sudo install -Dm0644 "$src" "$dest"
	info "Installed example config to ${dest}."
	info "Create your config by copying: cp ${dest} ${CONFIG_DIR}/config.json"
}

install_service() {
	if [ "$OS" != "linux" ]; then
		return 0
	fi

	if ! command -v systemctl >/dev/null 2>&1; then
		warn "systemd not detected — skipping service installation."
		return 0
	fi

	local src="$1"
	local dest="$SERVICE_DIR/$SERVICE_FILE"

	if [ -f "$dest" ]; then
		if cmp -s "$src" "$dest" >/dev/null 2>&1; then
			info "Service file ${dest} is already up to date — skipping."
			return 0
		fi
		printf "Systemd service file differs from bundled version. Replace? [y/N] "
		if ! confirm "Replace ${dest}?"; then
			info "Skipped service installation."
			return 0
		fi
	fi

	sudo install -Dm644 "$src" "$dest"
	sudo systemctl daemon-reload
	success "Installed ${SERVICE_FILE} to ${dest}."
}

main() {
	VERSION=""
	DRY_RUN=false
	BINARY_ONLY=false
	INSTALL_DIR="/usr/local/bin"
	OS=""
	ARCH=""

	while [ $# -gt 0 ]; do
		case "$1" in
			-v|--version) VERSION="$2"; shift ;;
			--dry-run)    DRY_RUN=true; shift ;;
			--binary-only) BINARY_ONLY=true; shift ;;
			-b|--bindir)  INSTALL_DIR="$2"; shift ;;
			-h|--help)
				echo "Usage: install.sh [options]"
				echo ""
				echo "Install Nenya AI Gateway from GitHub releases."
				echo ""
				echo "Options:"
				echo "  -v, --version <version>  Install specific version (default: latest)"
				echo "      --dry-run           Download and verify only, no filesystem changes"
				echo "      --binary-only       Install binary only, skip config and service"
				echo "  -b, --bindir <dir>    Install binary to <dir> (default: /usr/local/bin)"
				echo "  -h, --help            Show this help message"
				echo ""
				echo "After installation:"
				echo "  1. Create secrets: sudoedit ${CONFIG_DIR}/secrets.json"
				echo "  2. Enable service:   sudo systemctl enable --now nenya"
				echo "  3. Start:           sudo systemctl start nenya"
				echo "  4. Hot reload:       sudo systemctl reload nenya"
				echo ""
				echo "Install directory: ${INSTALL_DIR}"
				echo "Config directory:  ${CONFIG_DIR}"
				exit 0
				;;
			*) error "Unknown option: $1" ;;
		esac
	done

	check_deps
	check_go_version
	OS=$(detect_os)
	ARCH=$(detect_arch)
	resolve_version

	TMPDIR="$(mktemp -d)"
	trap cleanup EXIT

	BASE_URL="https://github.com/${GITHUB_REPO}/releases/download/${VERSION}"
	ARCHIVE="nenya-${VERSION#v}-${OS}-${ARCH}.tar.gz"
	CHECKSUMS="checksums.txt"

	info "Installing ${BIN_NAME} ${VERSION} (${OS}/${ARCH})"

	cd "$TMPDIR"

	info "Downloading checksums..."
	curl -sfLO "${BASE_URL}/${CHECKSUMS}" -o "$CHECKSUMS" || error "Failed to download checksums.txt."

	info "Downloading ${ARCHIVE}..."
	curl -sfLO "${BASE_URL}/${ARCHIVE}" -o "$ARCHIVE" || error "Failed to download ${ARCHIVE}."

	verify_download "$CHECKSUMS" "$ARCHIVE"

	if [ "$DRY_RUN" = true ]; then
		echo ""
		echo "Dry run complete. The following would be installed:"
		echo "  Binary:     ${INSTALL_DIR}/${BIN_NAME}"
		echo "  Config:     ${CONFIG_DIR}/config.json.example (if no config exists)"
		if [ "$OS" = "linux" ] && command -v systemctl >/dev/null 2>&1; then
			echo "  Service:    ${SERVICE_DIR}/${SERVICE_FILE} (if different from installed)"
		fi
		echo ""
		echo "No files were modified. Remove --dry-run to install."
		exit 0
	fi

	tar -xzf "$ARCHIVE" nenya "$SERVICE_FILE" config.json.example 2>/dev/null || true
	rm -f "$ARCHIVE" "$CHECKSUMS"

	install_binary "nenya"

	if [ "$BINARY_ONLY" = true ]; then
		echo ""
		info "Binary-only mode — skipping config and service setup."
		exit 0
	fi

	install_config "config.json.example"
	install_service "nenya.service"

	echo ""
	info "Installation complete. Next steps:"
	echo "  1. Create secrets: sudoedit ${CONFIG_DIR}/secrets.json"
	echo "     See docs/SECRETS_FORMAT.md for the required format."
	echo "  2. Enable service:   sudo systemctl enable --now nenya"
	echo "  3. Start:           sudo systemctl start nenya"
	echo " 4. Hot reload:       sudo systemctl reload nenya"
	echo ""
	echo "Install directory: ${INSTALL_DIR}"
	echo "Config directory: ${CONFIG_DIR}"
	echo "Service file:    ${SERVICE_DIR}/${SERVICE_FILE}"
}

main "$@"
