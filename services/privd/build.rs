// Compiles the shared contracts from ../../proto (the source of truth) into
// generated gRPC code. Requires protoc; the Makefile sets PROTOC to the
// repo-local toolchain binary.
fn main() -> Result<(), Box<dyn std::error::Error>> {
    let proto_root = "../../proto";
    println!("cargo:rerun-if-changed={proto_root}");

    tonic_prost_build::configure()
        .build_server(true)
        .build_client(true)
        .compile_protos(
            &[
                format!("{proto_root}/onyx/v1/health.proto"),
                format!("{proto_root}/onyx/v1/privd.proto"),
            ],
            &[proto_root.to_string()],
        )?;
    Ok(())
}