fn main() -> std::io::Result<()> {
    let proto = "../../proto/agentcomp/agentd/v1/agentd.proto";
    println!("cargo:rerun-if-changed={proto}");
    prost_build::Config::new()
        .bytes(["."])
        .compile_protos(&[proto], &["../../proto"])
}
