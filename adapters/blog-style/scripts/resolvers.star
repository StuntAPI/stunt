# Blog GraphQL resolvers — table-backed pattern with seeded collections.
#
# Root resolvers (on_<field>) read from store_collection("users"/"posts"/
# "comments"). Relational fields (resolve_<Type>_<field>) join across
# collections using foreign keys (user_id, post_id). Scalar fields use the
# default resolver (parent[fieldName]).
#
# All data is synthetic.

# ---------------------------------------------------------------------------
# Query root resolvers
# ---------------------------------------------------------------------------

# user(id) → User | None
def on_user(args):
    uid = args["args"]["id"]
    users = store_collection("users").list()
    for u in users:
        if u.get("id") == uid:
            return respond(200, u)
    return respond(200, None)

# users → [User]
def on_users(args):
    users = store_collection("users").list()
    return respond(200, users)

# post(id) → Post | None
def on_post(args):
    pid = args["args"]["id"]
    posts = store_collection("posts").list()
    for p in posts:
        if p.get("id") == pid:
            return respond(200, p)
    return respond(200, None)

# posts(status?) → [Post]  — optional filter by PostStatus enum.
def on_posts(args):
    status = args["args"].get("status")
    posts = store_collection("posts").list()
    if status != None:
        result = []
        for p in posts:
            if p.get("status") == status:
                result.append(p)
        return respond(200, result)
    return respond(200, posts)

# ---------------------------------------------------------------------------
# Mutation root resolvers
# ---------------------------------------------------------------------------

# createUser(name, bio?) → User
def on_createUser(args):
    a = args["args"]
    seq = store_kv_incr("blog", "user_seq")
    uid = "user-" + str(seq)
    user = {
        "id": uid,
        "name": a["name"],
        "bio": a.get("bio", None),
    }
    store_collection("users").insert(user)
    return respond(200, user)

# createPost(userId, title, body, status?) → Post
def on_createPost(args):
    a = args["args"]
    seq = store_kv_incr("blog", "post_seq")
    pid = "post-" + str(seq)
    status = a.get("status", "DRAFT")
    post = {
        "id": pid,
        "user_id": a["userId"],
        "title": a["title"],
        "body": a["body"],
        "status": status,
        "createdAt": "2024-03-01T12:00:00Z",
    }
    store_collection("posts").insert(post)
    return respond(200, post)

# addComment(postId, author, body) → Comment
def on_addComment(args):
    a = args["args"]
    seq = store_kv_incr("blog", "comment_seq")
    cid = "comment-" + str(seq)
    comment = {
        "id": cid,
        "post_id": a["postId"],
        "author": a["author"],
        "body": a["body"],
    }
    store_collection("comments").insert(comment)
    return respond(200, comment)

# ---------------------------------------------------------------------------
# Object resolvers — relational fields (the table-backed join pattern)
# ---------------------------------------------------------------------------

# User.posts → [Post]  — posts where user_id == parent["id"]
def resolve_User_posts(args):
    parent = args["parent"]
    uid = parent["id"]
    posts = store_collection("posts").list()
    result = []
    for p in posts:
        if p.get("user_id") == uid:
            result.append(p)
    return respond(200, result)

# Post.comments → [Comment]  — comments where post_id == parent["id"]
def resolve_Post_comments(args):
    parent = args["parent"]
    pid = parent["id"]
    comments = store_collection("comments").list()
    result = []
    for c in comments:
        if c.get("post_id") == pid:
            result.append(c)
    return respond(200, result)

# Post.author → User  — user where id == parent["user_id"]
def resolve_Post_author(args):
    parent = args["parent"]
    author_id = parent["user_id"]
    users = store_collection("users").list()
    for u in users:
        if u.get("id") == author_id:
            return respond(200, u)
    return respond(200, None)
