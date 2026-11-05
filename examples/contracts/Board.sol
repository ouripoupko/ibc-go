// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// A message board. Shows structs (returned as JSON objects), strings,
/// block.timestamp, and how an agent can learn its own address.
contract Board {
    struct Post {
        address author;
        uint256 time;
        string text;
    }

    Post[] private posts;
    mapping(address => uint256[]) private byAuthor;

    /// Publishes a post and returns its id.
    function post(string calldata text) external returns (uint256 id) {
        require(bytes(text).length > 0, "empty post");
        require(bytes(text).length <= 1000, "post too long");
        id = posts.length;
        posts.push(Post({author: msg.sender, time: block.timestamp, text: text}));
        byAuthor[msg.sender].push(id);
    }

    /// One post, by id.
    function getPost(uint256 id) external view returns (Post memory) {
        require(id < posts.length, "no such post");
        return posts[id];
    }

    /// The newest posts, newest first, at most limit of them.
    function latest(uint256 limit) external view returns (Post[] memory page) {
        uint256 n = limit < posts.length ? limit : posts.length;
        page = new Post[](n);
        for (uint256 i = 0; i < n; i++) {
            page[i] = posts[posts.length - 1 - i];
        }
    }

    /// The ids of the calling agent's posts.
    function myPosts() external view returns (uint256[] memory) {
        return byAuthor[msg.sender];
    }

    /// The number of posts.
    function count() external view returns (uint256) {
        return posts.length;
    }

    /// The calling agent's address (msg.sender), so a client can find it.
    function whoAmI() external view returns (address) {
        return msg.sender;
    }
}
