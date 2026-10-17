#!/usr/bin/env python3
"""Gitea-only image planning, build verification, and opt-in publication; no deployment."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import tempfile
from urllib.parse import urlsplit


ROOT = Path(__file__).resolve().parents[1]
PLAN = ROOT / "_out/container-plan.json"
VERIFIED = ROOT / "_out/container-verified.json"
VERSION_FILE = ROOT / "VERSION"
INPUTS = (
    "PUBLISH_ENABLED", "SOURCE_SERVER_URL", "SOURCE_REPOSITORY", "SOURCE_SHA",
    "SOURCE_REF", "SOURCE_EVENT", "REGISTRY_HOST", "IMAGE_PATH", "IMAGE_PLATFORM",
    "BUILD_IMAGE", "RUNTIME_IMAGE",
)
DIGEST = re.compile(r"sha256:[0-9a-f]{64}")
COMPONENT = r"[a-z0-9]+(?:[._-][a-z0-9]+)*"
NUMBER = r"(?:0|[1-9][0-9]*)"
SEMVER = re.compile(rf"{NUMBER}\.{NUMBER}\.{NUMBER}(?:-[a-z0-9-]+(?:\.[a-z0-9-]+)*)?")


def valid_semver(value):
    # Lowercase, registry-tag-compatible SemVer: no v prefix or +build metadata.
    # Numeric core/prerelease identifiers must not have leading zeroes.
    return (isinstance(value, str) and len(value) <= 127 and SEMVER.fullmatch(value) is not None
            and all(not (part.isdigit() and len(part) > 1 and part.startswith("0"))
                    for part in value.partition("-")[2].split(".")))


def read_version():
    try:
        version = VERSION_FILE.read_text().removesuffix("\n")
    except (OSError, UnicodeError):
        raise ValueError("Cannot read repository VERSION") from None
    if not valid_semver(version):
        raise ValueError("VERSION must contain lowercase MAJOR.MINOR.PATCH[-prerelease] without leading zeroes")
    return version


def registry_host(value):
    parsed = urlsplit("https://" + value)
    if (not value or not re.fullmatch(r"[a-z0-9.-]+(?::[0-9]+)?", value)
            or not parsed.hostname or parsed.username or parsed.path
            or parsed.query or parsed.fragment):
        raise ValueError("REGISTRY_HOST must be a hostname with optional port, not a URL")
    if parsed.port is not None and not 1 <= parsed.port <= 65535:
        raise ValueError("REGISTRY_HOST port is invalid")
    return value


def make_plan(values):
    values = {name: values.get(name, "") for name in INPUTS}
    if any(not isinstance(value, str) for value in values.values()):
        raise ValueError("Publication inputs must be strings")
    if values["PUBLISH_ENABLED"] != "true":
        raise ValueError("Set CONTAINER_PUBLISH_ENABLED=true to authorize publication")
    if values["SOURCE_EVENT"] not in ("push", "workflow_dispatch"):
        raise ValueError("Only trusted push/manual events may publish, never pull requests")
    ref = values["SOURCE_REF"]
    release = ""
    if ref != "refs/heads/main":
        release = ref.removeprefix("refs/tags/")
        if not ref.startswith("refs/tags/v") or not valid_semver(release.removeprefix("v")):
            raise ValueError("Publish only main or lowercase vMAJOR.MINOR.PATCH[-prerelease] tags")
    version = read_version()
    if release and release.removeprefix("v") != version:
        raise ValueError("Release tag must match repository VERSION")
    sha = values["SOURCE_SHA"]
    if not re.fullmatch(r"[0-9a-f]{40}", sha):
        raise ValueError("SOURCE_SHA must be the full lowercase Git commit SHA")
    server = urlsplit(values["SOURCE_SERVER_URL"])
    if (server.scheme != "https" or not server.hostname or server.username
            or server.password or server.query or server.fragment
            or re.search(r"\s", values["SOURCE_SERVER_URL"])):
        raise ValueError("SOURCE_SERVER_URL must be a credential-free HTTPS Gitea URL")
    registry = registry_host((values["REGISTRY_HOST"] or server.netloc).lower())
    repository = values["SOURCE_REPOSITORY"]
    if not re.fullmatch(COMPONENT + "/" + COMPONENT, repository.lower()):
        raise ValueError("SOURCE_REPOSITORY must be owner/repository")
    path = values["IMAGE_PATH"] or repository.lower()
    if not re.fullmatch(COMPONENT + r"(?:/" + COMPONENT + r")+", path) or path.split("/")[0] != repository.lower().split("/")[0]:
        raise ValueError("IMAGE_PATH must be lowercase and remain under the repository owner")
    platform = values["IMAGE_PLATFORM"] or "linux/amd64"
    if platform not in ("linux/amd64", "linux/arm64"):
        raise ValueError("IMAGE_PLATFORM must be one native Linux amd64 or arm64 platform")
    for name in ("BUILD_IMAGE", "RUNTIME_IMAGE"):
        image = values[name]
        reference, separator, digest = image.rpartition("@")
        if (not separator or not DIGEST.fullmatch(digest) or not reference
                or not re.fullmatch(r"[a-z0-9.-]+(?::[0-9]+)?/" + COMPONENT +
                                    r"(?:/" + COMPONENT + r")*(?::[a-z0-9][a-z0-9._-]*)?", reference)):
            raise ValueError(f"{name} must be a reviewed image reference pinned with @sha256:<digest>")
    image = registry + "/" + path
    tags = [image + ":sha-" + sha]
    if release:
        tags.append(image + ":" + release)
    return {"inputs": values, "registry": registry, "image": image, "tags": tags,
            "platform": platform, "revision": sha, "version": version,
            "source": values["SOURCE_SERVER_URL"].rstrip("/") + "/" + repository}


def docker_failure_hint(output):
    # Return only fixed hints, never excerpts of daemon replies/endpoints/tokens.
    output = output.lower()
    if "permission denied" in output or "access is denied" in output:
        return "Docker socket/API access denied; check the job user's socket group/permissions, not chmod 666"
    if re.search(r"client.*(?:too old|too new|newer than server)|minimum supported api version|maximum supported api version", output):
        return "Docker client/API version mismatch; align Docker CLI/Engine and check any DOCKER_API_VERSION override"
    if "x509:" in output or "tls handshake" in output or "tls: bad certificate" in output:
        return "Docker daemon TLS validation failed; configure trusted CA/client certificates without disabling TLS"
    if "context" in output and ("not found" in output or "does not exist" in output):
        return "Docker context unavailable in the job; use the approved default socket or verified DOCKER_HOST connection"
    if "no such host" in output or "name or service not known" in output:
        return "Docker daemon hostname could not be resolved from the job"
    if "timeout" in output or "timed out" in output or "deadline exceeded" in output:
        return "Docker daemon connection timed out; check the dedicated daemon's reachability"
    if ("cannot connect to the docker daemon" in output or "is the docker daemon running" in output
            or "connection refused" in output or "no such file or directory" in output):
        return "No reachable Docker daemon from this job; check socket forwarding or the approved DOCKER_HOST connection"
    return "Docker preflight failed for an unclassified reason; inspect the worker locally without sharing credentials"


def docker_connection_summary():
    host = os.environ.get("DOCKER_HOST", "")
    # Do not print raw environment values, hostnames, context names or CA paths.
    transport = host.split(":", 1)[0] if host else "default"
    if transport not in ("default", "unix", "tcp", "ssh", "http", "https", "npipe"):
        transport = "other"
    summary = {"uid": os.getuid(), "gid": os.getgid(), "groups": os.getgroups(),
               "docker_host_transport": transport}
    for name in ("DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_API_VERSION", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"):
        summary[name + "_configured"] = bool(os.environ.get(name))
    socket = Path(host.removeprefix("unix://")) if host.startswith("unix://") else Path("/var/run/docker.sock")
    try:
        metadata = socket.stat()
        summary["socket_probe"] = {"present": True, "is_socket": stat.S_ISSOCK(metadata.st_mode),
                                   "uid": metadata.st_uid, "gid": metadata.st_gid,
                                   "mode": format(stat.S_IMODE(metadata.st_mode), "04o"),
                                   "readable": os.access(socket, os.R_OK), "writable": os.access(socket, os.W_OK)}
    except FileNotFoundError:
        summary["socket_probe"] = {"present": False}
    except OSError:
        summary["socket_probe"] = {"probe_failed": True}
    return summary


def run(args, *, env=None, capture=False, stdin=None, timeout=None):
    # No shell expansion; registry credentials never become command arguments.
    environment = dict(os.environ if env is None else env)
    environment.pop("REGISTRY_TOKEN", None)
    environment.pop("REGISTRY_USERNAME", None)
    result = subprocess.run(args, cwd=ROOT, env=environment, input=stdin, text=True,
                            capture_output=capture, timeout=timeout)
    if result.returncode:
        if capture and (args[:2] in (["docker", "info"], ["docker", "version"])
                        or args[:3] == ["docker", "buildx", "version"]):
            hint = docker_failure_hint((result.stderr or "")[:32768])
            raise ValueError(f"Docker preflight failed (exit {result.returncode}): {hint}; raw output suppressed")
        raise ValueError(f"{args[0]} {args[1]} failed; review the runner step (captured output suppressed)")
    return result.stdout if capture else ""


def verify_checkout(plan):
    if run(["git", "rev-parse", "HEAD"], capture=True).strip() != plan["revision"]:
        raise ValueError("Checked-out commit differs from the planned publication commit")


def write_json(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("x") as output:
        json.dump(value, output, sort_keys=True, indent=2)


def load_plan():
    plan = json.loads(PLAN.read_text())
    if (not isinstance(plan, dict) or not isinstance(plan.get("inputs"), dict)
            or make_plan(plan["inputs"]) != plan):
        raise ValueError("Invalid or altered container publication plan")
    verify_checkout(plan)
    return plan


def plan_digest(plan):
    return hashlib.sha256(json.dumps(plan, sort_keys=True).encode()).hexdigest()


def inspect_image(reference, env=None):
    images = json.loads(run(["docker", "image", "inspect", reference], env=env, capture=True))
    if not isinstance(images, list) or len(images) != 1 or not isinstance(images[0], dict):
        raise ValueError("Docker did not return one image identity")
    image = images[0]
    if not DIGEST.fullmatch(image.get("Id", "")):
        raise ValueError("Docker returned an invalid image identity")
    return image


def isolated_docker_environment(directory, registry):
    # A nonempty auths map also stops Docker auto-selecting an installed global
    # credential helper. Login must remain in this disposable config file only.
    config = Path(directory) / "config.json"
    config.write_text(json.dumps({"auths": {registry: {}}}))
    config.chmod(0o600)
    return dict(os.environ, DOCKER_CONFIG=directory)


def docker_preflight():
    try:
        run(["docker", "buildx", "version"], capture=True, timeout=30)
        daemon = json.loads(run(["docker", "info", "--format", "{{json .}}"], capture=True, timeout=30))
        if not isinstance(daemon, dict):
            raise ValueError("Docker did not return daemon platform information")
        return daemon
    except (ValueError, OSError, subprocess.TimeoutExpired):
        print("Docker connection diagnostics: " + json.dumps(docker_connection_summary(), sort_keys=True), flush=True)
        raise


def build(plan):
    if VERIFIED.exists():
        raise ValueError("Previous image verification exists; use a fresh CI workspace")
    daemon = docker_preflight()
    architecture = {"x86_64": "amd64", "aarch64": "arm64"}.get(daemon.get("Architecture"), daemon.get("Architecture"))
    if daemon.get("OSType") != "linux" or plan["platform"] != "linux/" + str(architecture):
        raise ValueError("IMAGE_PLATFORM must match the Docker daemon for native image verification")
    # Empty, isolated client config: never inherit a worker's registry login.
    with tempfile.TemporaryDirectory(prefix="provider-build-", dir=os.getenv("RUNNER_TEMP")) as directory:
        env = isolated_docker_environment(directory, plan["registry"])
        command = ["docker", "buildx", "build", "--builder", "default", "--pull",
                   "--load", "--platform", plan["platform"], "--progress", "plain",
                   "--provenance=false", "--sbom=false"]
        arguments = {"BUILD_IMAGE": plan["inputs"]["BUILD_IMAGE"],
                     "RUNTIME_IMAGE": plan["inputs"]["RUNTIME_IMAGE"],
                     "IMAGE_SOURCE": plan["source"], "IMAGE_VERSION": plan["version"],
                     "IMAGE_REVISION": plan["revision"]}
        for name, value in arguments.items():
            command.extend(["--build-arg", name + "=" + value])
        for tag in plan["tags"]:
            command.extend(["--tag", tag])
        run(command + ["."], env=env)
        image = inspect_image(plan["tags"][0], env)
        config = image.get("Config", {})
        if (config.get("User") != "65532:65532"
                or config.get("Entrypoint") != ["/usr/local/bin/provider"]
                or f"{image.get('Os')}/{image.get('Architecture')}" != plan["platform"]):
            raise ValueError("Built image has unexpected user, entrypoint or platform")
        output = run(["docker", "run", "--rm", "--network", "none", "--read-only",
                      "--cap-drop", "ALL", "--security-opt", "no-new-privileges=true",
                      "--user", "65532:65532", "--tmpfs", "/tmp:rw,noexec,nosuid,size=64m,mode=1777",
                      image["Id"], "--help"], env=env, capture=True, timeout=60)
        if "--omni-api-endpoint" not in output:
            raise ValueError("Built image did not return provider CLI help")
        version = run(["docker", "run", "--rm", "--network", "none", "--read-only",
                       "--cap-drop", "ALL", "--security-opt", "no-new-privileges=true",
                       "--user", "65532:65532", "--tmpfs", "/tmp:rw,noexec,nosuid,size=64m,mode=1777",
                       image["Id"], "--version"], env=env, capture=True, timeout=60)
        if version.strip() != "provider version " + plan["version"]:
            raise ValueError("Container binary version does not match the publication plan")
    write_json(VERIFIED, {"plan_digest": plan_digest(plan), "image_id": image["Id"], "provider_version": plan["version"]})
    print("Container image verified; registry authentication has not been used.")


def publish(plan):
    verified = json.loads(VERIFIED.read_text())
    if not isinstance(verified, dict) or verified.get("plan_digest") != plan_digest(plan):
        raise ValueError("Publication plan differs from the verified build plan")
    if verified.get("provider_version") != plan["version"]:
        raise ValueError("Publication requires a matching verified binary version")
    for tag in plan["tags"]:
        if inspect_image(tag)["Id"] != verified.get("image_id"):
            raise ValueError("Publication tag no longer refers to the verified image")
    username, token = os.environ.get("REGISTRY_USERNAME", ""), os.environ.get("REGISTRY_TOKEN", "")
    if not username or not token or re.search(r"\s", username) or "\n" in token or "\r" in token:
        raise ValueError("Set valid REGISTRY_USERNAME and REGISTRY_TOKEN Actions secrets")
    # Only this temporary config holds the login; delete it on success/failure.
    with tempfile.TemporaryDirectory(prefix="provider-registry-", dir=os.getenv("RUNNER_TEMP")) as directory:
        env = isolated_docker_environment(directory, plan["registry"])
        run(["docker", "login", plan["registry"], "--username", username, "--password-stdin"],
            env=env, capture=True, stdin=token + "\n", timeout=60)
        for tag in plan["tags"]:
            run(["docker", "push", tag], env=env)
        image = inspect_image(verified["image_id"], env)
        digests = [value for value in image.get("RepoDigests", [])
                   if value.startswith(plan["image"] + "@") and DIGEST.fullmatch(value.split("@")[-1])]
        if len(set(digests)) != 1:
            raise ValueError("Push finished but one unambiguous registry digest could not be verified")
    print("Published tags: " + ", ".join(plan["tags"]))
    print("Pin this image for a separately approved deployment: " + digests[0])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("plan", "doctor", "build", "publish"))
    args = parser.parse_args()
    try:
        if args.command == "plan":
            plan = make_plan(os.environ)
            verify_checkout(plan)
            write_json(PLAN, plan)
            print("Validated image tags: " + ", ".join(plan["tags"]))
        elif args.command == "doctor":
            docker_preflight()
            print("Docker daemon and Buildx are reachable; no image build or registry login performed.")
        elif args.command == "build":
            build(load_plan())
        else:
            publish(load_plan())
    except (ValueError, OSError, subprocess.TimeoutExpired) as error:
        # Error strings deliberately omit secret values and captured CLI output.
        parser.exit(1, f"Container CI failed: {error}\n")


if __name__ == "__main__":
    main()
