use contenox_e2e::{Instance, Script, ToolCall, Turn};
use std::time::Duration;

#[test]
fn run_puts_the_scripted_report_on_stdout_and_exits_zero() {
    let cx = Instance::named("run-smoke").expect("scratch instance");
    cx.init().ok();

    let script = Script::new()
        .turn(
            Turn::new()
                .text("Filing what I found.")
                .call(ToolCall::mission_report(
                    "result",
                    "scripted run reporting home",
                )),
        )
        .turn(Turn::new().call(ToolCall::mission_finish("landed")))
        .turn("Mission finished.");
    cx.scripted(&script).expect("scripted-test backend");

    let out = cx
        .cmd(["run", "--policy", "run", "report what you know"])
        .timeout(Duration::from_secs(180))
        .output()
        .expect("contenox run")
        .ok()
        .expect_stdout("scripted run reporting home")
        .expect_stderr("finished: landed")
        .refute_stdout("Mission fired at agent");

    assert_eq!(out.stdout.trim(), "scripted run reporting home");

    let missions = cx.missions().expect("contenox mission list");
    assert_eq!(missions.len(), 1, "one mission, got {missions:?}");
    assert_eq!(missions[0].agent, "run");
    assert_eq!(missions[0].envelope, "run");
    assert_eq!(missions[0].status, "landed");

    let shown = cx
        .mission_show(&missions[0].id)
        .expect("contenox mission show");
    assert_eq!(shown.get("Intent"), "report what you know");
    assert_eq!(shown.get("Status"), "landed");
}
