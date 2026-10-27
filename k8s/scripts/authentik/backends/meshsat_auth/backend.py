"""A password backend that admits only a waiting MeshSat applicant (MESHSAT-1719).

The MeshSat sign-in (flow meshsat-authentication, its own password stage
meshsat-authentication-password) shows a waiting applicant who typed the RIGHT
password a "waiting for approval" page instead of "Failed to authenticate."
(MESHSAT-1717). authentik's inbuilt backend refuses every inactive account,
the same as a wrong password, so the stage needs one more backend that lets
the applicant's correct password through.

Django's AllowAllUsersModelBackend did that for EVERY inactive account on this
shared instance, the other product's included: a correct password then gave a
page of its own, which tells a guesser the password is right. This backend
accepts an inactive account only when its only group is meshsat-pending, the
state the MeshSat enrolment flow leaves an applicant in until a person approves
it. Any other inactive account fails here exactly like a wrong password.

It runs after the inbuilt backend, so an active account never gets here. The
caller (the identification stage) still meets the MeshSat flow's own steps: the
waiting applicant is sent to the notice and no session is ever created, because
authentik's login stage refuses an inactive user whatever a backend says.

Source of truth: meshsat-hub k8s/scripts/authentik/backends/meshsat_auth/. A
copy is mounted from a ConfigMap in the omoikane repo (k8s/auth/meshsat-auth/,
OMOIKANE-1679) at /meshsat_auth, which imports as meshsat_auth.backend because
the server runs from /. If the mount is missing, authentik logs "Failed to
import backend" and skips it: applicants see "Failed to authenticate." again,
and nothing is admitted.
"""

from authentik.core.auth import InbuiltBackend

PENDING_GROUP = "meshsat-pending"


class PendingApplicantBackend(InbuiltBackend):
    """InbuiltBackend for inactive accounts whose only group is meshsat-pending."""

    def user_can_authenticate(self, user):
        if getattr(user, "is_active", True):
            # Active accounts are the inbuilt backend's, which runs first.
            return False
        names = list(user.groups.values_list("name", flat=True))
        return names == [PENDING_GROUP]
