// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// A chat channel, for an app: users let the app post and like for them
/// without approving each message (their node signs those calls), while
/// changing the topic is approved every time. Anyone may join as a guest:
/// guests post, like and read through the partners' nodes, without taking
/// part in consensus.
///
/// A user may make their copy public: then anyone reads the topic and the
/// number of messages without signing in (msg.sender is the zero address),
/// but not the messages themselves.
contract Chat {
    struct Message {
        address author;
        uint256 time;
        string text;
        uint256 likes;
    }

    address public owner;
    string public topic;
    Message[] private messages;

    constructor(string memory initialTopic) {
        owner = msg.sender;
        topic = initialTopic;
    }

    /// Who may join as a guest: anyone. (Without this function, no one may.)
    function approve_guest(address) external pure returns (bool) {
        return true;
    }

    /// Posts a message and returns its id.
    function post(string calldata text) external returns (uint256 id) {
        require(bytes(text).length > 0, "empty message");
        require(bytes(text).length <= 1000, "message too long");
        id = messages.length;
        messages.push(Message({author: msg.sender, time: block.timestamp, text: text, likes: 0}));
    }

    /// Likes a message; returns its number of likes.
    function like(uint256 id) external returns (uint256) {
        require(id < messages.length, "no such message");
        return ++messages[id].likes;
    }

    /// Changes the topic. Only the owner.
    function setTopic(string calldata newTopic) external {
        require(msg.sender == owner, "only the owner sets the topic");
        topic = newTopic;
    }

    /// The newest messages, newest first, at most limit of them. Not for
    /// unknown readers, even when the user made the contract public.
    function latest(uint256 limit) external view returns (Message[] memory page) {
        require(msg.sender != address(0), "sign in to read the messages");
        uint256 n = limit < messages.length ? limit : messages.length;
        page = new Message[](n);
        for (uint256 i = 0; i < n; i++) {
            page[i] = messages[messages.length - 1 - i];
        }
    }

    /// The number of messages.
    function count() external view returns (uint256) {
        return messages.length;
    }
}
