// Auto-refresh for the job table.
//
// The table fragment is swapped wholesale, so refreshing at a fixed interval
// fights with whatever the operator is doing: it closes an open edit form
// mid-typing, drops a selection, and can cancel a click that is still in
// flight (htmx abandons a request whose issuing element leaves the DOM).
//
// So the timer lives here instead of in an "every 3s" trigger, and it only
// fires when the UI is genuinely idle.

const REFRESH_MS = 3000;

// Number of htmx requests currently in flight. A refresh during one of them is
// exactly the race that made the Retry button look broken.
let inFlight = 0;

function jobsSection() {
    return document.getElementById("jobs");
}

function selectedBoxes() {
    return document.querySelectorAll("#jobs input[name='ids']:checked");
}

// busyReason returns why a refresh must be skipped, or "" when it may proceed.
function busyReason() {
    const jobs = jobsSection();
    if (!jobs) {
        return "no table";
    }
    // Nothing is queued or running: no reason to keep asking.
    if (jobs.dataset.active !== "true") {
        return "idle";
    }
    if (inFlight > 0) {
        return "request in flight";
    }
    if (selectedBoxes().length > 0) {
        return "selection";
    }
    // An open "Edit query" form is unsaved work; never swap it away.
    if (jobs.querySelector("details.edit[open]")) {
        return "editing";
    }
    const active = document.activeElement;
    if (active && jobs.contains(active) && active.matches("input, textarea, button, summary")) {
        return "focus";
    }
    return "";
}

function tick() {
    if (busyReason() === "") {
        document.body.dispatchEvent(new CustomEvent("refreshJobs"));
    }
    updateHint();
}

function updateHint() {
    const hint = document.getElementById("selection-hint");
    if (!hint) {
        return;
    }

    const count = selectedBoxes().length;
    if (count > 0) {
        hint.textContent = `${count} selected — auto-refresh paused`;
        return;
    }

    switch (busyReason()) {
        case "editing":
        case "focus":
            hint.textContent = "auto-refresh paused while editing";
            break;
        case "idle":
            hint.textContent = "";
            break;
        default:
            hint.textContent = "";
    }
}

function toggleAll(source) {
    document
        .querySelectorAll("#jobs input[name='ids']")
        .forEach((box) => {
            box.checked = source.checked;
        });
    updateHint();
}

// Exposed for the checkbox onchange handlers in the table.
function refreshSelection() {
    updateHint();
}

document.addEventListener("htmx:beforeRequest", () => {
    inFlight += 1;
});

// afterRequest fires for both success and failure; afterOnLoad would not.
document.addEventListener("htmx:afterRequest", () => {
    inFlight = Math.max(0, inFlight - 1);
});

document.addEventListener("htmx:afterSwap", updateHint);
document.addEventListener("DOMContentLoaded", () => {
    updateHint();
    setInterval(tick, REFRESH_MS);
});
