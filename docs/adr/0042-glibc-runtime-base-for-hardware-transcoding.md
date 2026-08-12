# The Docker runtime base is glibc (Debian), because hardware transcoding requires it

The official image's runtime stage is `debian:trixie-slim`, not Alpine, and installs the VA drivers and oneVPL runtime alongside `ffmpeg`.

Hardware acceleration is only as real as the container it runs in. [ADR-0009](./0009-transcode-governance.md) makes hardware accel opt-in with a guaranteed CPU fallback and validates the operator's choice at startup; that validation is honest, so a runtime image missing the encoder simply reports the fallback forever. The image must therefore carry the userspace for every backend ADR-0009 names, or the knob is decorative.

## Why

The Alpine-based image could not run a single Linux hardware backend, and the startup validation dutifully fell back to CPU on all of them:

- **NVENC** — Alpine's `ffmpeg` package is built with no `h264_nvenc` at all, so step 1 of validation (encoder compiled into ffmpeg) could never pass.
- **VAAPI / QSV** — the encoders *were* compiled in, but `apk add ffmpeg` installs `libva` with no VA driver behind it: `/usr/lib/dri` was empty. `vaInitialize` therefore failed and step 2 (a real one-frame test-encode) could never pass, on any card.

Rebuilding ffmpeg with `--enable-nvenc` would have fixed only the first bullet, and only on paper. The NVIDIA Container Toolkit works by bind-mounting the **host's** driver libraries into the container — `libnvidia-encode.so`, `libcuda.so`, `nvidia-smi` — and those are glibc-linked. They cannot be loaded by a musl process, which is why NVIDIA supports Debian/Ubuntu/RHEL base images and not Alpine. No amount of ffmpeg rebuilding changes that: **glibc is a hard requirement for NVENC in a container**, so the base image had to move.

Debian was chosen over Ubuntu for being the smaller of the two and for shipping `ffmpeg` with `h264_nvenc`, `h264_vaapi`, and `h264_qsv` in `main`. Trixie over bookworm for ffmpeg 7.x, whose QSV path uses the current oneVPL dispatcher rather than the deprecated MSDK.

The one thing an image genuinely cannot solve is device permissions. `/dev/dri/renderD*` is `root:render 0660` and the host's `render` GID is host-specific, so the image creates the group and puts the runtime user in it — but operators still pass `--group-add` for their own GID. Documented rather than worked around; running the server as root to dodge it would be a worse trade.

## Consequences

- **The image is roughly 3.5× larger.** The runtime base went from ~52 MB (Alpine + ffmpeg) to ~185 MB (Debian + ffmpeg + Mesa + Intel media drivers + oneVPL), for a ~217 MB final image. This is the deliberate cost of an image where the hardware knob works; a "minimal" image that silently CPU-transcodes is not actually cheaper for the operator paying for it in watts.
- **`NVIDIA_DRIVER_CAPABILITIES=compute,video,utility` is baked into the image.** `--gpus all` chooses visible *devices* and does not widen capabilities, and the default set omits `video` — without which `libnvidia-encode` is never injected and NVENC fails validation on a perfectly good host. It is inert when no NVIDIA runtime is present.
- **Non-free Intel drivers stay out.** `intel-media-va-driver-non-free` decodes more formats but is not in Debian `main`; keeping the official image free-only is the default, and a derived image can add it.
- ADR-0009's warn-and-fall-back posture is what made this survivable rather than an outage: every affected operator got working CPU playback and a startup warning, not a broken server. The bug was that the warning was unavoidable.
