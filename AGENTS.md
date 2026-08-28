# Repository Guidelines

## Project Structure & Module Organization

This Rust 2021 project is an RCD serial acquisition gateway that publishes data through OPC UA and TCP. The binary entry point is `src/main.rs`; reusable modules are exposed from `src/lib.rs`. Core modules live under `src/`: `calc_engine/` handles expression parsing and evaluation, `serial/` handles port configuration and workers, `server/` contains OPC UA/TCP services, and `model/` defines shared domain types. Integration tests are in `tests/`, with fixtures in `test_data/`. Tooling and packaging helpers live in `tools/`; `tools/nesting-check/` is a sibling Rust tool. `serialport-rs/` is a local path dependency. Treat `target/`, `musl_output/`, `tools/build/`, and `tools/output/` as generated artifacts.

## Build, Test, and Development Commands

- `cargo build`: compile the main crate in debug mode.
- `cargo run`: run the gateway locally; ensure required runtime config files and PKI material are available first.
- `cargo test`: run unit tests in `src/` and integration tests in `tests/`.
- `cargo test calc_engine_comprehensive`: run the main calculation-engine integration suite.
- `cargo fmt`: format Rust code with rustfmt.
- `cargo clippy --all-targets --all-features`: run lint checks across library, binary, and tests.
- `.\build_musl.ps1`: build a static Linux musl release in Docker and copy it to `musl_output/rcd_server`.

## Coding Style & Naming Conventions

Use standard Rust formatting with 4-space indentation via `cargo fmt`. Prefer `snake_case` for functions, modules, variables, and tests; use `PascalCase` for structs, enums, and traits. Keep module boundaries aligned with the current layout. Use `thiserror` or typed errors where possible, and keep logging through `log` macros (`info!`, `warn!`, `error!`).

## Testing Guidelines

Add focused unit tests beside the module under test using `#[test]` or `#[tokio::test]`. Put cross-module tests in `tests/`, and keep fixtures in `test_data/`. For calculation behavior, update `tests/calc_engine_comprehensive.rs` and compare against `tests/calc_expected_oracle.py` or CSV fixtures when relevant.

## Commit & Pull Request Guidelines

Recent history uses concise conventional-style subjects such as `fix: stabilize OPC startup data sync`, `feat: ...`, and `chore: ...`. Keep the first line imperative and scoped to the change. Pull requests should describe behavior changes, list validation commands run, link related issues, and call out config, PKI, serial-port, or protocol compatibility impacts.

## Security & Configuration Tips

Do not commit new private keys, certificates, device configs, or environment-specific serial settings unless explicitly intended. Review changes under `pki/`, `calc.toml`, and runtime config files carefully because they can affect deployed gateway behavior.
